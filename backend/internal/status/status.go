// Package status builds the zero-dependency fleet health view served at /status and
// /api/v1/status: one row per configured room, read straight from the broker's in-memory
// telemetry state (no database, no Grafana/Prometheus dependency).
package status

import (
	"fmt"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

// Device is one room's point-in-time health, derived from the latest telemetry report.
type Device struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	// Label is the panel's position within a room ("Door", "Interior wall"), set only when a room
	// has more than one display. Empty for the common single-display room. Callers rendering a
	// heading should use Title(), which folds it into Name.
	Label             string `json:"label,omitempty"`
	BatteryPct        int    `json:"battery_pct"`
	BatteryMV         int    `json:"battery_mv"`
	LastSeenSeconds   int64  `json:"last_seen_seconds"`
	LastRenderSeconds *int64 `json:"last_render_seconds,omitempty"` // omitted if never reported
	RSSI              int    `json:"rssi"`
	FirmwareVersion   string `json:"firmware_version"`
	BootCount         int    `json:"boot_count"`
	// Status is one of "ok" | "stale" | "low_battery" | "unreported", derived from the same
	// thresholds notify.Manager alerts on (config.AlertConfig) so this view and the building-
	// manager alert never disagree about the same device.
	Status string `json:"status"`
}

// Title is the heading a human should see for this display: just the room name when the room has
// one panel, "Room — Label" when it has several. Keeping this in one place means /status,
// /dashboard, and any alert text can never disagree about how a second panel is named.
func (d Device) Title() string {
	if d.Label == "" {
		return d.Name
	}
	return d.Name + " — " + d.Label
}

// LastRenderDisplay formats LastRenderSeconds for the human-readable /status page.
func (d Device) LastRenderDisplay() string {
	if d.LastRenderSeconds == nil {
		return "never"
	}
	return fmt.Sprintf("%ds ago", *d.LastRenderSeconds)
}

// Build derives one Device per configured room from the current telemetry snapshot.
func Build(cfg *config.Config, tlm *telemetry.Store, now time.Time) []Device {
	out := make([]Device, 0, len(cfg.Rooms))
	for _, room := range cfg.Rooms {
		d := Device{DeviceID: room.DeviceID, Name: room.Name, Label: room.Label}

		snap, ok := tlm.Snapshot(room.DeviceID)
		if !ok {
			d.Status = "unreported"
			out = append(out, d)
			continue
		}

		d.BatteryPct = snap.Report.BatteryPct
		d.BatteryMV = snap.Report.BatteryMV
		d.RSSI = snap.Report.RSSI
		d.FirmwareVersion = snap.Report.FirmwareVer
		d.BootCount = snap.Report.BootCount
		d.LastSeenSeconds = int64(now.Sub(snap.LastSeen).Seconds())
		if !snap.LastRendered.IsZero() {
			v := int64(now.Sub(snap.LastRendered).Seconds())
			d.LastRenderSeconds = &v
		}

		switch {
		case now.Sub(snap.LastSeen) > StaleThreshold(cfg, snap):
			d.Status = "stale"
		case snap.Report.BatteryPct > 0 && snap.Report.BatteryPct <= cfg.Alerts.LowBatteryPct:
			d.Status = "low_battery"
		default:
			d.Status = "ok"
		}
		out = append(out, d)
	}
	return out
}

// StaleThreshold is how long a device may go without reporting before it counts as stale: two of
// its own expected sleeps (the clamped next-wake the broker last sent it), so a flat room on a
// 10-min cycle and a smart room sleeping 6h are each judged against their own schedule. Falls back
// to alerts.stale_after when the expected sleep isn't known (no display fetch since broker start).
// Must match the DisplayStale PrometheusRule in deploy/k3s/alerts.yaml.
func StaleThreshold(cfg *config.Config, snap telemetry.Snapshot) time.Duration {
	if snap.ExpectedWake > 0 {
		return 2 * time.Duration(snap.ExpectedWake) * time.Second
	}
	return cfg.Alerts.StaleAfter
}
