// Package notify delivers fleet alerts (low battery, display offline/recovered) to the selected
// channels (Lark, Web Push, legacy Slack) and applies per-device hysteresis + rate-limiting so
// the building manager gets one actionable message per transition — not a ping on every
// 10-minute wake at 44%, or on every check of a display that's still offline.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Message is a rendered alert ready to dispatch.
// Body is the message as one text: the title, then the body on its own line when there is one.
func (m Message) Body() string {
	if m.Text == "" {
		return m.Title
	}
	return m.Title + "\n" + m.Text
}

type Message struct {
	Title    string
	Text     string
	DeviceID string
	Room     string
	// Location is the room's office (config Location ID): Lark DMs that location's person and
	// browser push reaches only that location's subscribers. "" = no location routing.
	Location string
}

// Notifier sends a Message to some external channel.
type Notifier interface {
	Send(ctx context.Context, m Message) error
	Name() string
}

// ── Slack incoming webhook ───────────────────────────────────────────────────
// Posts the standard Slack {"text": ...} payload to an incoming-webhook URL.
type SlackWebhook struct {
	URL    string
	Client *http.Client
}

func NewSlackWebhook(url string) *SlackWebhook {
	return &SlackWebhook{URL: url, Client: &http.Client{Timeout: 8 * time.Second}}
}

func (s *SlackWebhook) Name() string { return "slack-webhook" }

func (s *SlackWebhook) Send(ctx context.Context, m Message) error {
	text := "*" + m.Title + "*"
	if m.Text != "" {
		text += "\n" + m.Text
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return redactURL(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return redactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}

// redactURL strips the request URL from a transport error. net/http wraps failures in *url.Error,
// whose message is `Post "<full URL>": <cause>` — and for these channels the URL is the secret
// (a Lark or Slack webhook) or a per-browser capability (a push endpoint). Without this, a failed
// send would print it into logs and the /admin "send test" result.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s request failed: %w", ue.Op, ue.Err)
	}
	return err
}

// logNotifier is the fallback when no webhook is configured: alerts still surface in logs
// (and via the Prometheus path), they just don't get pushed anywhere.
type logNotifier struct{ log *slog.Logger }

func (l *logNotifier) Name() string { return "log" }
func (l *logNotifier) Send(_ context.Context, m Message) error {
	l.log.Warn("ALERT (no webhook configured)", "title", m.Title, "text", m.Text)
	return nil
}

// Multi fans a Message out to several channels. Every channel is attempted even if an earlier
// one fails; failures are joined and attributed by channel name.
type Multi []Notifier

func (m Multi) Name() string {
	names := make([]string, len(m))
	for i, n := range m {
		names[i] = n.Name()
	}
	return strings.Join(names, "+")
}

func (m Multi) Send(ctx context.Context, msg Message) error {
	var errs []error
	for _, n := range m {
		if err := n.Send(ctx, msg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// ── Hysteresis manager ───────────────────────────────────────────────────────
type devState struct {
	alerting     bool // currently below the low threshold (latched)
	lastNotified time.Time
}

// Settings are the alert manager's tunables. They are read live (see SetSettings), so a change made
// in /admin applies to the next alert without a restart.
type Settings struct {
	LowPct   int           // alert at/below this battery percent
	ClearPct int           // re-arm at/above this
	Renotify time.Duration // repeat an alert for a still-bad display no sooner than this
	Style    string        // message wording: "plain" (default), "minimal" or "detailed"
}

type Manager struct {
	notifier Notifier
	lowPct   int
	clearPct int
	renotify time.Duration
	settings func() Settings // nil = the values the manager was built with
	log      *slog.Logger
	mu       sync.Mutex
	state    map[string]*devState
	stale    map[string]*devState // per-device offline latch, independent of the battery latch
	locate   func(deviceID string) string
}

// SetSettings makes the manager read its thresholds and message style from f on every evaluation,
// so editing them in /admin takes effect immediately instead of at the next restart.
func (m *Manager) SetSettings(f func() Settings) {
	m.mu.Lock()
	m.settings = f
	m.mu.Unlock()
}

// current returns the settings in force. Caller holds m.mu.
func (m *Manager) current() Settings {
	cur := Settings{LowPct: m.lowPct, ClearPct: m.clearPct, Renotify: m.renotify}
	if m.settings == nil {
		return cur
	}
	got := m.settings()
	// A zero threshold means "not set": keep the value the manager was built with rather than
	// treating 0% / 0s as an instruction.
	if got.LowPct == 0 {
		got.LowPct = cur.LowPct
	}
	if got.ClearPct == 0 {
		got.ClearPct = cur.ClearPct
	}
	if got.Renotify == 0 {
		got.Renotify = cur.Renotify
	}
	return got
}

// SetLocator tells the manager how to find a device's location, so every alert it sends is routed
// to the right office. Set by the server from the live config.
func (m *Manager) SetLocator(f func(deviceID string) string) {
	m.mu.Lock()
	m.locate = f
	m.mu.Unlock()
}

// NewManager builds the alert manager with the legacy single-channel wiring: Slack if webhookURL
// is set, otherwise log only. Kept for existing callers; NewManagerWith takes explicit channels.
func NewManager(webhookURL string, lowPct, clearPct int, renotify time.Duration, log *slog.Logger) *Manager {
	var n Notifier
	if webhookURL != "" {
		n = NewSlackWebhook(webhookURL)
	}
	return NewManagerWith(n, lowPct, clearPct, renotify, log)
}

// NewManagerWith builds the alert manager around n (typically a Multi of the selected channels).
// A nil n means no channel is selected: alerts are logged and go nowhere else.
func NewManagerWith(n Notifier, lowPct, clearPct int, renotify time.Duration, log *slog.Logger) *Manager {
	if n == nil {
		n = &logNotifier{log: log}
	}
	log.Info("alert manager ready", "notifier", n.Name(), "low_pct", lowPct, "clear_pct", clearPct)
	return &Manager{
		notifier: n, lowPct: lowPct, clearPct: clearPct, renotify: renotify,
		log: log, state: make(map[string]*devState), stale: make(map[string]*devState),
	}
}

// Notifier returns the channel fan-out alerts go to (for the admin "send test" action).
func (m *Manager) Notifier() Notifier {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notifier
}

// SetNotifier swaps the channel fan-out (nil = log only). main builds Web Push after the server
// exists, because the server is what stores the subscriptions.
func (m *Manager) SetNotifier(n Notifier) {
	if n == nil {
		n = &logNotifier{log: m.log}
	}
	m.mu.Lock()
	m.notifier = n
	m.mu.Unlock()
	m.log.Info("alert channels", "notifier", n.Name())
}

// EvaluateBattery is called on every telemetry report. It fires an alert once when a device
// crosses below lowPct, re-fires only after `renotify` if it stays low, and re-arms once the
// device recovers above clearPct (hysteresis). Dispatch is async so it never blocks the device.
// Returns whether an alert was fired (used by tests; callers may ignore it).
func (m *Manager) EvaluateBattery(deviceID, room string, pct, mv int, now time.Time) bool {
	if pct < 0 || pct > 100 {
		return false // unknown/garbage reading — don't alert
	}
	m.mu.Lock()
	st := m.state[deviceID]
	if st == nil {
		st = &devState{}
		m.state[deviceID] = st
	}

	cfg := m.current()
	var fire bool
	switch {
	case pct <= cfg.LowPct:
		if !st.alerting || now.Sub(st.lastNotified) >= cfg.Renotify {
			fire = true
			st.alerting = true
			st.lastNotified = now
		}
	case pct >= cfg.ClearPct:
		st.alerting = false // recovered (charged) → re-arm for the next downward crossing
	}
	m.mu.Unlock()

	if !fire {
		return false
	}
	name := room
	if name == "" {
		name = deviceID
	}
	title, text := batteryText(cfg.Style, name, deviceID, pct, mv)
	msg := Message{Title: title, Text: text, DeviceID: deviceID, Room: room}
	m.log.Warn("low battery alert", "device", deviceID, "room", room, "pct", pct, "mv", mv)
	m.dispatch(msg)
	return true
}

// StaleEvent is what EvaluateStale decided for one check.
type StaleEvent int

const (
	StaleNone      StaleEvent = iota
	StaleOffline              // went offline, or still offline and renotify elapsed
	StaleRecovered            // reported again after an offline alert
)

// EvaluateStale is called periodically for every device that has reported since start, with
// whether it is currently past its stale threshold (status.StaleThreshold: two of its own
// expected sleeps). Same hysteresis shape as EvaluateBattery: one "offline" message on the
// transition, a repeat only after `renotify` while it stays offline, and one "recovered"
// message when it reports again — never one per check.
func (m *Manager) EvaluateStale(deviceID, room string, stale bool, silentFor time.Duration, now time.Time) StaleEvent {
	m.mu.Lock()
	st := m.stale[deviceID]
	if st == nil {
		st = &devState{}
		m.stale[deviceID] = st
	}
	cfg := m.current()
	ev := StaleNone
	switch {
	case stale && (!st.alerting || now.Sub(st.lastNotified) >= cfg.Renotify):
		ev = StaleOffline
		st.alerting = true
		st.lastNotified = now
	case !stale && st.alerting:
		ev = StaleRecovered
		st.alerting = false
	}
	m.mu.Unlock()

	name := room
	if name == "" {
		name = deviceID
	}
	switch ev {
	case StaleOffline:
		m.log.Warn("display offline alert", "device", deviceID, "room", room, "silent_for", silentFor.Round(time.Minute))
		title, text := offlineText(cfg.Style, name, deviceID, silentFor)
		m.dispatch(Message{Title: title, Text: text, DeviceID: deviceID, Room: room})
	case StaleRecovered:
		m.log.Info("display recovered", "device", deviceID, "room", room)
		title, text := recoveredText(cfg.Style, name, deviceID)
		m.dispatch(Message{Title: title, Text: text, DeviceID: deviceID, Room: room})
	}
	return ev
}

// dispatch sends asynchronously so alert delivery never blocks a device request or the checker.
func (m *Manager) dispatch(msg Message) {
	n := m.Notifier()
	m.mu.Lock()
	if msg.Location == "" && m.locate != nil {
		msg.Location = m.locate(msg.DeviceID)
	}
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := n.Send(ctx, msg); err != nil {
			m.log.Error("alert dispatch failed", "device", msg.DeviceID, "err", err)
		}
	}()
}

// Message styles. "plain" is the default: what is wrong and what to do about it, in words anyone can
// read. "minimal" is one short line. "detailed" adds the device ID and the raw readings.
const (
	StylePlain    = "plain"
	StyleMinimal  = "minimal"
	StyleDetailed = "detailed"
)

// NormalizeStyle maps anything unset or unknown to the default, so a typo can never silence alerts.
func NormalizeStyle(s string) string {
	switch s {
	case StyleMinimal, StyleDetailed:
		return s
	}
	return StylePlain
}

func batteryText(style, name, deviceID string, pct, mv int) (title, text string) {
	switch NormalizeStyle(style) {
	case StyleMinimal:
		return fmt.Sprintf("🔋 %s: battery %d%%", name, pct), ""
	case StyleDetailed:
		return fmt.Sprintf("🔋 Low battery: %s (%d%%)", name, pct),
			fmt.Sprintf("Display %q (device %s) is at %d%% (%d mV). Please recharge/replace soon.\nNote: LiPo %%→V is approximate near this range; %d mV is the raw reading.", name, deviceID, pct, mv, mv)
	}
	return fmt.Sprintf("🔋 Low battery: %s (%d%%)", name, pct), "Please recharge or replace it soon."
}

func offlineText(style, name, deviceID string, silentFor time.Duration) (title, text string) {
	switch NormalizeStyle(style) {
	case StyleMinimal:
		return fmt.Sprintf("📵 %s: offline", name), ""
	case StyleDetailed:
		return fmt.Sprintf("📵 Display offline: %s", name),
			fmt.Sprintf("Display %q (device %s) hasn't checked in for %s — two of its scheduled check-ins. Check its battery and Wi-Fi.", name, deviceID, silentFor.Round(time.Minute))
	}
	return fmt.Sprintf("📵 Display offline: %s", name),
		fmt.Sprintf("It hasn't checked in for %s. Check its battery and Wi-Fi.", plainDuration(silentFor))
}

func recoveredText(style, name, deviceID string) (title, text string) {
	switch NormalizeStyle(style) {
	case StyleMinimal:
		return fmt.Sprintf("✅ %s: back online", name), ""
	case StyleDetailed:
		return fmt.Sprintf("✅ Display back online: %s", name),
			fmt.Sprintf("Display %q (device %s) is checking in again.", name, deviceID)
	}
	return fmt.Sprintf("✅ Display back online: %s", name), ""
}

// plainDuration is a duration a person would say: "40 minutes", "about 3 hours", "about 2 days".
func plainDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		n := int(d.Round(time.Minute) / time.Minute)
		if n < 1 {
			n = 1
		}
		if n == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", n)
	case d < 48*time.Hour:
		n := int(d.Round(time.Hour) / time.Hour)
		if n == 1 {
			return "about 1 hour"
		}
		return fmt.Sprintf("about %d hours", n)
	}
	return fmt.Sprintf("about %d days", int(d.Round(24*time.Hour)/(24*time.Hour)))
}
