package server

import (
	"context"
	"fmt"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// BuildNotifier turns cfg's selected alert channels into one notify.Notifier (nil = log only).
// It errors — and main refuses to start — when a selected channel can't actually work, e.g. a
// VAPID private key that doesn't match its public key. config.Validate has already rejected a
// selected channel whose secret is unset.
func (s *Server) BuildNotifier(cfg *config.Config) (notify.Notifier, error) {
	var out notify.Multi
	for _, ch := range cfg.Alerts.EffectiveChannels() {
		switch ch {
		case config.ChannelLark:
			out = append(out, notify.NewLarkWebhook(cfg.Alerts.LarkWebhookURL))
		case config.ChannelSlack:
			out = append(out, notify.NewSlackWebhook(cfg.Alerts.WebhookURL))
		case config.ChannelWebPush:
			keys, err := notify.ParseVAPIDKeys(cfg.Alerts.WebPush.VAPIDPublicKey, cfg.Alerts.WebPush.VAPIDPrivateKey)
			if err != nil {
				return nil, fmt.Errorf("alerts.webpush: %w", err)
			}
			s.vapidPublicKey = keys.PublicKey()
			out = append(out, notify.NewWebPush(keys, cfg.Alerts.WebPush.Subject, s))
		default:
			return nil, fmt.Errorf("alerts.channels: unknown channel %q", ch)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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
