package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
)

// The /admin "Notifications" section: which channels alerts go to, a "send test" per channel,
// and whether each channel's secret is present. Secrets themselves are never rendered or
// accepted here — they live in the deployment's Secret and reach the config as ${VAR}
// references; this page only ever shows a set/unset boolean.

// channelStatus is one row of the panel.
type channelStatus struct {
	Name      string // config.Channel*
	Label     string
	Selected  bool
	SecretSet bool
	SecretVar string // the env var(s) an operator sets — named, never valued
}

type notificationsPanel struct {
	Channels         []channelStatus
	MySubscriptions  int // the signed-in admin's browsers with push enabled
	AllSubscriptions int
	Tested           string // channel just tested successfully, for the confirmation line
}

func buildNotificationsPanel(cfg *config.Config, username, tested string) notificationsPanel {
	selected := cfg.Alerts.EffectiveChannels()
	a := cfg.Alerts
	rows := []channelStatus{
		{Name: config.ChannelLark, Label: "Lark", SecretSet: a.LarkWebhookURL != "", SecretVar: "LARK_WEBHOOK_URL"},
		{Name: config.ChannelWebPush, Label: "Browser notifications", SecretVar: "WEBPUSH_VAPID_PUBLIC_KEY, WEBPUSH_VAPID_PRIVATE_KEY",
			SecretSet: a.WebPush.VAPIDPublicKey != "" && a.WebPush.VAPIDPrivateKey != "" && a.WebPush.Subject != ""},
	}
	// Slack is shown only where it's already in use, so legacy configs can turn it off here.
	if a.WebhookURL != "" || slices.Contains(selected, config.ChannelSlack) {
		rows = append(rows, channelStatus{Name: config.ChannelSlack, Label: "Slack (legacy)", SecretSet: a.WebhookURL != "", SecretVar: "ALERT_WEBHOOK_URL"})
	}
	for i := range rows {
		rows[i].Selected = slices.Contains(selected, rows[i].Name)
	}
	return notificationsPanel{
		Channels:         rows,
		MySubscriptions:  len(cfg.PushSubscriptionsFor(username)),
		AllSubscriptions: len(cfg.PushSubscriptions),
		Tested:           tested,
	}
}

func adminRedirectError(w http.ResponseWriter, r *http.Request, msg, anchor string) {
	http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(msg)+"#"+anchor, http.StatusSeeOther)
}

// handleAdminSaveNotifications saves the channel checkboxes. Fail-closed end to end: a channel
// whose secret is unset is rejected by config validation, a channel that can't actually be built
// (e.g. a mismatched VAPID pair) is rejected before anything is persisted, and the new selection
// only goes live after it has been saved.
func (s *Server) handleAdminSaveNotifications(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var channels []string
	for _, ch := range []string{config.ChannelLark, config.ChannelWebPush, config.ChannelSlack} {
		if slices.Contains(r.PostForm["channel"], ch) {
			channels = append(channels, ch)
		}
	}
	if len(channels) == 0 {
		channels = []string{config.ChannelLog} // explicit none; see config.ChannelLog
	}
	cfg := s.cfg.Load()
	alerts := cfg.Alerts
	alerts.Channels = channels
	newCfg, err := cfg.WithAlerts(alerts)
	if err != nil {
		s.log.Error("admin notifications save rejected", "err", err)
		adminRedirectError(w, r, "Couldn't turn that channel on: its secret isn't set on the server.||"+err.Error(), "notifications")
		return
	}
	if _, _, err := buildChannels(newCfg, newCfg.Alerts.EffectiveChannels(), s); err != nil {
		adminRedirectError(w, r, "That channel's configuration doesn't work.||"+err.Error(), "notifications")
		return
	}
	if err := s.applyConfig(r.Context(), newCfg, false); err != nil {
		s.log.Error("admin notifications save failed to persist", "err", err)
		adminRedirectError(w, r, err.Error(), "notifications")
		return
	}
	if err := s.ActivateChannels(newCfg); err != nil { // already built once above; can't differ
		s.log.Error("admin notifications activate failed", "err", err)
	}
	s.log.Info("alert channels changed from /admin", "channels", channels)
	http.Redirect(w, r, "/admin?saved=notifications#notifications", http.StatusSeeOther)
}

// userPushSubs scopes Web Push to one user's browsers, so "send test" reaches only the admin who
// clicked it. Expired subscriptions it finds are still cleaned up fleet-wide.
type userPushSubs struct {
	s        *Server
	username string
}

func (u userPushSubs) PushSubscriptions() []notify.PushSubscription {
	var out []notify.PushSubscription
	for _, sub := range u.s.cfg.Load().PushSubscriptionsFor(u.username) {
		out = append(out, notify.PushSubscription{Endpoint: sub.Endpoint, P256DH: sub.P256DH, Auth: sub.Auth})
	}
	return out
}

func (u userPushSubs) RemovePushSubscription(endpoint string) { u.s.RemovePushSubscription(endpoint) }

// handleAdminTestNotification sends one test message on one channel, synchronously, and reports
// the outcome. Works for a channel that isn't selected yet (so it can be checked before turning
// it on) as long as its secret is set. Browser push goes only to the clicking admin's browsers.
func (s *Server) handleAdminTestNotification(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// test_channel, not channel: the test buttons live inside the channel form, so the ticked
	// checkboxes ("channel") are submitted too.
	ch := r.PostForm.Get("test_channel")
	cfg := s.cfg.Load()
	user := s.sessionUsername(adminUI, r)

	var row *channelStatus
	panel := buildNotificationsPanel(cfg, user, "")
	for i := range panel.Channels {
		if panel.Channels[i].Name == ch {
			row = &panel.Channels[i]
		}
	}
	if row == nil {
		http.Error(w, "unknown channel", http.StatusBadRequest)
		return
	}
	if !row.SecretSet {
		adminRedirectError(w, r, fmt.Sprintf("Can't test %s: its secret (%s) isn't set on the server.", row.Label, row.SecretVar), "notifications")
		return
	}
	if ch == config.ChannelWebPush && panel.MySubscriptions == 0 {
		adminRedirectError(w, r, "Turn on browser notifications for this browser first (link in this section), then test.", "notifications")
		return
	}
	n, _, err := buildChannels(cfg, []string{ch}, userPushSubs{s: s, username: user})
	if err != nil {
		adminRedirectError(w, r, "That channel's configuration doesn't work.||"+err.Error(), "notifications")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err = n.Send(ctx, notify.Message{
		Title: "🔔 Test notification from the meeting-display broker",
		Text:  fmt.Sprintf("Sent from /admin by %s at %s. If you can read this, %s alerts work.", user, time.Now().UTC().Format("15:04 UTC"), row.Label),
	})
	if err != nil { // errors are URL-redacted in notify, so no webhook or endpoint can leak here
		s.log.Error("admin test notification failed", "channel", ch, "err", err)
		adminRedirectError(w, r, fmt.Sprintf("The %s test didn't go through.", row.Label)+"||"+err.Error(), "notifications")
		return
	}
	s.log.Info("admin test notification sent", "channel", ch, "user", user)
	http.Redirect(w, r, "/admin?saved=notifications&tested="+template.URLQueryEscaper(ch)+"#notifications", http.StatusSeeOther)
}
