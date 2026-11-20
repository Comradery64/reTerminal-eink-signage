package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

// Three failing reports from one display reach the admins, and the status API shows the error.
// (2026-11: four units failed every repaint for weeks and nothing anywhere said so.)
func TestTelemetryErrorsAlertAdminsAndShowInStatus(t *testing.T) {
	s := testServer()
	rec := &recordingNotifier{}
	s.alerts.SetNotifier(rec)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	post := func(body string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/telemetry/rt-1", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("telemetry: err=%v resp=%v", err, resp)
		}
	}
	for i := 0; i < 3; i++ {
		post(`{"batt_pct":80,"batt_mv":3900,"err":"render","rendered":false}`)
	}
	waitFor(t, func() bool { return len(rec.titles()) >= 1 })
	rec.mu.Lock()
	m := rec.msgs[0]
	rec.mu.Unlock()
	if !m.Admin || !strings.Contains(m.Title, "not updating") || !strings.Contains(m.Text, "faded") {
		t.Fatalf("want an admin 'not updating' alert explaining the faded screen, got %+v", m)
	}

	resp, err := http.Get(srv.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	var devs []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&devs)
	if len(devs) == 0 || devs[0]["last_error"] != "render" || devs[0]["consecutive_errors"] != float64(3) {
		t.Fatalf("status API: want last_error=render consecutive_errors=3, got %v", devs)
	}

	post(`{"batt_pct":80,"batt_mv":3900,"rendered":true}`)
	waitFor(t, func() bool { return len(rec.titles()) >= 2 })
	if got := rec.titles()[1]; !strings.Contains(got, "updating again") {
		t.Fatalf("a clean report must say the display recovered, got %q", got)
	}
}

// Operational alerts reach admin accounts' browsers only; battery news still reaches everyone.
func TestPushSubscriptionsAdminOnly(t *testing.T) {
	s := testServerWithAuth(t)
	c := s.cfg.Load()
	for _, user := range []string{testAdminUsername, testManagerUsername} {
		p, a := browserKeys(t)
		var err error
		if c, err = c.WithPushSubscription(config.PushSubscription{Username: user, Endpoint: "https://push.example/" + user, P256DH: p, Auth: a}); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.Store(c)
	if got := s.PushSubscriptions("", true); len(got) != 1 || got[0].Endpoint != "https://push.example/"+testAdminUsername {
		t.Fatalf("adminOnly: want only the admin's browser, got %v", got)
	}
	if got := s.PushSubscriptions("", false); len(got) != 2 {
		t.Fatalf("everyone: want 2 browsers, got %d", len(got))
	}
}

// alerts.offline_min_silence raises the bar: a 10-minute display is "two check-ins late" at 20
// minutes, but with a 30m minimum the admins hear nothing until 30 minutes of silence.
func TestOfflineMinSilence(t *testing.T) {
	s := testServer()
	rec := &recordingNotifier{}
	s.alerts.SetNotifier(rec)
	c := *s.cfg.Load()
	c.Alerts.OfflineMinSilence = 30 * time.Minute
	s.cfg.Store(&c)
	t0 := time.Now()
	s.tlm.SetExpectedWake("rt-1", 600)
	s.tlm.Ingest("rt-1", telemetry.Report{BatteryPct: 80}, t0)

	s.checkStale(t0.Add(25 * time.Minute)) // past two check-ins, under the 30-minute minimum
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.titles()); n != 0 {
		t.Fatalf("25 minutes of silence must not alert with a 30m minimum, got %d", n)
	}
	s.checkStale(t0.Add(31 * time.Minute))
	waitFor(t, func() bool { return len(rec.titles()) >= 1 })
	if got := rec.titles()[0]; !strings.Contains(got, "offline") {
		t.Fatalf("want an offline alert at 31 minutes, got %q", got)
	}
}
