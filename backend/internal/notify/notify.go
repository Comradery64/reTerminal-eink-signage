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
	"strings"
	"sync"
	"time"
)

// Message is a rendered alert ready to dispatch.
type Message struct {
	Title    string
	Text     string
	DeviceID string
	Room     string
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
	body, _ := json.Marshal(map[string]string{"text": "*" + m.Title + "*\n" + m.Text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
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

type Manager struct {
	notifier Notifier
	lowPct   int
	clearPct int
	renotify time.Duration
	log      *slog.Logger
	mu       sync.Mutex
	state    map[string]*devState
	stale    map[string]*devState // per-device offline latch, independent of the battery latch
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

	var fire bool
	switch {
	case pct <= m.lowPct:
		if !st.alerting || now.Sub(st.lastNotified) >= m.renotify {
			fire = true
			st.alerting = true
			st.lastNotified = now
		}
	case pct >= m.clearPct:
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
	msg := Message{
		Title:    fmt.Sprintf("🔋 Low battery: %s (%d%%)", name, pct),
		Text:     fmt.Sprintf("Display %q (device %s) is at %d%% (%d mV). Please recharge/replace soon.\nNote: LiPo %%→V is approximate near this range; %d mV is the raw reading.", name, deviceID, pct, mv, mv),
		DeviceID: deviceID,
		Room:     room,
	}
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
	ev := StaleNone
	switch {
	case stale && (!st.alerting || now.Sub(st.lastNotified) >= m.renotify):
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
		m.dispatch(Message{
			Title:    fmt.Sprintf("📵 Display offline: %s", name),
			Text:     fmt.Sprintf("Display %q (device %s) hasn't checked in for %s — two of its scheduled check-ins. Check its battery and Wi-Fi.", name, deviceID, silentFor.Round(time.Minute)),
			DeviceID: deviceID, Room: room,
		})
	case StaleRecovered:
		m.log.Info("display recovered", "device", deviceID, "room", room)
		m.dispatch(Message{
			Title:    fmt.Sprintf("✅ Display back online: %s", name),
			Text:     fmt.Sprintf("Display %q (device %s) is checking in again.", name, deviceID),
			DeviceID: deviceID, Room: room,
		})
	}
	return ev
}

// dispatch sends asynchronously so alert delivery never blocks a device request or the checker.
func (m *Manager) dispatch(msg Message) {
	n := m.Notifier()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := n.Send(ctx, msg); err != nil {
			m.log.Error("alert dispatch failed", "device", msg.DeviceID, "err", err)
		}
	}()
}
