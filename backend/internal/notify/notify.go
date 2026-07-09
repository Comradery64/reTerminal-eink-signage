// Package notify delivers fleet alerts (currently low-battery) to an external channel and
// applies per-device hysteresis + rate-limiting so the building manager gets one actionable
// message on the downward crossing — not a ping on every 10-minute wake at 44%.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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

// ── Hysteresis manager ───────────────────────────────────────────────────────
type devState struct {
	alerting     bool      // currently below the low threshold (latched)
	lastNotified time.Time
}

type Manager struct {
	notifier   Notifier
	lowPct     int
	clearPct   int
	renotify   time.Duration
	log        *slog.Logger
	mu         sync.Mutex
	state      map[string]*devState
}

// NewManager builds the alert manager. If webhookURL is empty, alerts only log.
func NewManager(webhookURL string, lowPct, clearPct int, renotify time.Duration, log *slog.Logger) *Manager {
	var n Notifier = &logNotifier{log: log}
	if webhookURL != "" {
		n = NewSlackWebhook(webhookURL)
	}
	log.Info("alert manager ready", "notifier", n.Name(), "low_pct", lowPct, "clear_pct", clearPct)
	return &Manager{
		notifier: n, lowPct: lowPct, clearPct: clearPct, renotify: renotify,
		log: log, state: make(map[string]*devState),
	}
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.notifier.Send(ctx, msg); err != nil {
			m.log.Error("alert dispatch failed", "device", deviceID, "err", err)
		}
	}()
	return true
}
