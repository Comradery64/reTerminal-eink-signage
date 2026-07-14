package status

import (
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

func testConfig() *config.Config {
	return &config.Config{
		Alerts: config.AlertConfig{LowBatteryPct: 45, StaleAfter: time.Hour},
		Rooms: []config.Room{
			{DeviceID: "rt-1", Name: "Aspen"},
			{DeviceID: "rt-2", Name: "Birch"},
			{DeviceID: "rt-3", Name: "Cedar"},
		},
	}
}

func TestBuildClassifiesEachDevice(t *testing.T) {
	cfg := testConfig()
	tlm := telemetry.New()
	now := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)

	// rt-1: healthy, has rendered recently.
	tlm.Ingest("rt-1", telemetry.Report{BatteryPct: 80, RenderedBool: true}, now.Add(-time.Minute))
	// rt-2: low battery, checked in recently.
	tlm.Ingest("rt-2", telemetry.Report{BatteryPct: 30}, now.Add(-time.Minute))
	// rt-3: never reported at all.

	devices := Build(cfg, tlm, now)
	if len(devices) != 3 {
		t.Fatalf("want 3 devices, got %d", len(devices))
	}

	byID := map[string]Device{}
	for _, d := range devices {
		byID[d.DeviceID] = d
	}

	if got := byID["rt-1"].Status; got != "ok" {
		t.Errorf("rt-1 status = %q, want ok", got)
	}
	if byID["rt-1"].LastRenderSeconds == nil {
		t.Error("rt-1 should have a last-render reading")
	}
	if got := byID["rt-2"].Status; got != "low_battery" {
		t.Errorf("rt-2 status = %q, want low_battery", got)
	}
	if got := byID["rt-3"].Status; got != "unreported" {
		t.Errorf("rt-3 status = %q, want unreported", got)
	}
}

func TestBuildStaleOverridesLowBattery(t *testing.T) {
	cfg := testConfig()
	tlm := telemetry.New()
	now := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)

	tlm.Ingest("rt-1", telemetry.Report{BatteryPct: 10}, now.Add(-2*time.Hour))

	devices := Build(cfg, tlm, now)
	if devices[0].Status != "stale" {
		t.Errorf("status = %q, want stale (staleness should take priority)", devices[0].Status)
	}
}
