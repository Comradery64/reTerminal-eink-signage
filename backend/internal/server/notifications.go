package server

import (
	"context"
	"fmt"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// buildChannels turns the named channels into one notify.Notifier (nil = log only) plus the
// Web Push public key to advertise ("" if webpush isn't among them). Pure: it changes nothing, so
// /admin can validate a selection before saving it, and "send test" can build a single channel.
// subs is where Web Push reads subscriptions from (the whole fleet's, or one admin's for a test).
// It errors when a channel can't actually work, e.g. a VAPID private key that doesn't match its
// public key; config.Validate has already rejected a channel whose secret is unset.
func buildChannels(cfg *config.Config, channels []string, subs notify.PushSubscriptions) (notify.Notifier, string, error) {
	var out notify.Multi
	vapid := ""
	for _, ch := range channels {
		switch ch {
		case config.ChannelLark:
			out = append(out, notify.NewLarkWebhook(cfg.Alerts.LarkWebhookURL))
		case config.ChannelSlack:
			out = append(out, notify.NewSlackWebhook(cfg.Alerts.WebhookURL))
		case config.ChannelLog:
			// explicit "no channels": nothing to build, alerts are logged only
		case config.ChannelWebPush:
			keys, err := notify.ParseVAPIDKeys(cfg.Alerts.WebPush.VAPIDPublicKey, cfg.Alerts.WebPush.VAPIDPrivateKey)
			if err != nil {
				return nil, "", fmt.Errorf("alerts.webpush: %w", err)
			}
			vapid = keys.PublicKey()
			out = append(out, notify.NewWebPush(keys, cfg.Alerts.WebPush.Subject, subs))
		default:
			return nil, "", fmt.Errorf("alerts.channels: unknown channel %q", ch)
		}
	}
	if len(out) == 0 {
		return nil, "", nil
	}
	return out, vapid, nil
}

// ActivateChannels makes cfg's selected channels the live alert destinations — at startup (main
// exits if it errors: fail closed) and after an /admin channel change.
func (s *Server) ActivateChannels(cfg *config.Config) error {
	n, vapid, err := buildChannels(cfg, cfg.Alerts.EffectiveChannels(), s)
	if err != nil {
		return err
	}
	s.derivedMu.Lock()
	s.vapidPublicKey = vapid
	s.derivedMu.Unlock()
	s.alerts.SetNotifier(n)
	return nil
}

// vapidKey is the applicationServerKey browsers subscribe with; "" when webpush is off.
func (s *Server) vapidKey() string {
	s.derivedMu.RLock()
	defer s.derivedMu.RUnlock()
	return s.vapidPublicKey
}

// PushSubscriptions implements notify.PushSubscriptions from the live config.
func (s *Server) PushSubscriptions() []notify.PushSubscription {
	subs := s.cfg.Load().PushSubscriptions
	out := make([]notify.PushSubscription, len(subs))
	for i, sub := range subs {
		out[i] = notify.PushSubscription{Endpoint: sub.Endpoint, P256DH: sub.P256DH, Auth: sub.Auth}
	}
	return out
}

// RemovePushSubscription implements notify.PushSubscriptions: the push service said the
// subscription is gone (404/410), so drop it from the config instead of retrying it forever.
func (s *Server) RemovePushSubscription(endpoint string) {
	next, removed := s.cfg.Load().WithoutPushSubscription(endpoint)
	if !removed {
		return
	}
	if err := s.applyConfig(context.Background(), next, false); err != nil {
		s.log.Error("could not remove expired push subscription", "err", err)
		return
	}
	s.log.Info("removed expired push subscription", "push_service", originOf(endpoint))
}

// RunStaleChecks evaluates every reported device against its own stale threshold
// (status.StaleThreshold — the same rule as /api/v1/status and the DisplayStale alert) every
// interval, and lets the alert manager decide whether that's an offline or recovered message.
// Staleness is the absence of reports, so it has to be checked on a clock; nothing else would
// notice a display that has simply gone quiet.
func (s *Server) RunStaleChecks(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkStale(s.now())
		}
	}
}

func (s *Server) checkStale(now time.Time) {
	cfg := s.cfg.Load()
	for _, room := range cfg.Rooms {
		snap, ok := s.tlm.Snapshot(room.DeviceID)
		if !ok {
			continue // never reported since start: unknown, not offline (same as /metrics)
		}
		silent := now.Sub(snap.LastSeen)
		s.alerts.EvaluateStale(room.DeviceID, room.Name, silent > status.StaleThreshold(cfg, snap), silent, now)
	}
}
