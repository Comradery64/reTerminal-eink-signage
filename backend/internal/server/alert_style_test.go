package server

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func saveAlerts(t *testing.T, s *Server, low, clear, style string) string {
	t.Helper()
	rec := adminPost(t, s.Handler(), s, "/admin/alerts/save", url.Values{
		"low_battery_pct": {low}, "clear_pct": {clear}, "min_renotify": {"24h"}, "stale_after": {"1h"}, "message_style": {style}})
	return rec.Header().Get("Location")
}

// Alert thresholds saved in /admin apply to the next alert without a restart. (They used to be read
// only when the broker started, so the form saved a value that did nothing.)
func TestAdminAlertThresholdsApplyWithoutRestart(t *testing.T) {
	s := testServerWithAuth(t) // manager built with low=45
	if loc := saveAlerts(t, s, "60", "70", "plain"); strings.Contains(loc, "error") {
		t.Fatalf("save failed: %s", loc)
	}
	if !s.alerts.EvaluateBattery("rt-1", "Aspen", 55, 3700, time.Now()) {
		t.Fatal("a 55% battery must alert once the threshold is saved as 60%")
	}
}

// The message style chosen in /admin is the wording of the next alert, and a bad value is refused.
func TestAdminMessageStyleChangesNextAlert(t *testing.T) {
	s := testServerWithAuth(t)
	rec := &recordingNotifier{}
	s.alerts.SetNotifier(rec)
	now := time.Now()

	s.alerts.EvaluateBattery("rt-1", "Aspen", 40, 3690, now)
	waitFor(t, func() bool { return len(rec.titles()) >= 1 })
	if m := rec.msgs[0]; strings.Contains(m.Text, "mV") || strings.Contains(m.Text, "rt-1") || m.Text != "Please recharge or replace it soon." {
		t.Fatalf("the default style must be plain, got %q", m.Text)
	}

	if loc := saveAlerts(t, s, "45", "55", "minimal"); strings.Contains(loc, "error") {
		t.Fatalf("save: %s", loc)
	}
	s.alerts.EvaluateStale("rt-1", "Aspen", true, time.Hour, now)
	waitFor(t, func() bool { return len(rec.titles()) >= 2 })
	if got := rec.titles()[1]; got != "📵 Aspen: offline" {
		t.Fatalf("after choosing minimal the next alert read %q", got)
	}

	if loc := saveAlerts(t, s, "45", "55", "shouting"); !strings.Contains(loc, "error") {
		t.Error("an unknown style must be refused")
	}
	if got := s.cfg.Load().Alerts.MessageStyle; got != "minimal" {
		t.Errorf("a refused save changed the stored style to %q", got)
	}
}

// /admin offers the three styles with the current one selected (plain when nothing is set).
func TestAdminPageOffersMessageStyles(t *testing.T) {
	s := testServerWithAuth(t)
	page := adminGet(t, s.Handler(), s, "/admin")
	for _, want := range []string{`name="message_style"`, `value="plain" selected`, `value="minimal" `, `value="detailed" `} {
		if !strings.Contains(page, want) {
			t.Errorf("/admin Alerts is missing %s", want)
		}
	}
	saveAlerts(t, s, "45", "55", "detailed")
	if page := adminGet(t, s.Handler(), s, "/admin"); !strings.Contains(page, `value="detailed" selected`) {
		t.Error("the saved style is not the selected one")
	}
}
