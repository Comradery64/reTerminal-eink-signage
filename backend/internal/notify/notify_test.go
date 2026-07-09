package notify

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func newTestManager() *Manager {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// low=45, clear=55, renotify=24h — matches the broker defaults.
	return NewManager("", 45, 55, 24*time.Hour, log)
}

func TestBatteryHysteresis(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)

	step := func(min int, pct int, want bool) {
		t.Helper()
		got := m.EvaluateBattery("rt-1", "Aspen", pct, 3500, t0.Add(time.Duration(min)*time.Minute))
		if got != want {
			t.Fatalf("at +%dmin pct=%d: fired=%v want=%v", min, pct, got, want)
		}
	}

	step(0, 60, false)  // healthy
	step(10, 46, false) // just above threshold
	step(20, 44, true)  // ← downward crossing: fire once
	step(30, 43, false) // still low, within renotify window: silent
	step(40, 40, false) // still low: silent
	step(50, 70, false) // charged past clear (55): re-arm, no alert on recovery
	step(60, 44, true)  // ← crosses low again after recovery: fire again
}

func TestBatteryRenotifyAfterWindow(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)

	if !m.EvaluateBattery("rt-2", "Birch", 40, 3450, t0) {
		t.Fatal("expected first alert at 40%")
	}
	// Still low 1h later — inside the 24h window, must stay silent.
	if m.EvaluateBattery("rt-2", "Birch", 38, 3440, t0.Add(time.Hour)) {
		t.Fatal("should not re-alert within renotify window")
	}
	// 25h later, still low — past the window, remind once.
	if !m.EvaluateBattery("rt-2", "Birch", 38, 3440, t0.Add(25*time.Hour)) {
		t.Fatal("expected re-alert after renotify window")
	}
}

func TestBatteryIgnoresGarbage(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)
	if m.EvaluateBattery("rt-3", "Cedar", -1, 0, t0) {
		t.Fatal("must not alert on unknown (-1) reading")
	}
	if m.EvaluateBattery("rt-3", "Cedar", 250, 0, t0) {
		t.Fatal("must not alert on out-of-range reading")
	}
}
