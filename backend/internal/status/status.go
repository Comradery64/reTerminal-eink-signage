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
	DeviceID          string `json:"device_id"`
	Name              string `json:"name"`
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
		d := Device{DeviceID: room.DeviceID, Name: room.Name}

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
		case now.Sub(snap.LastSeen) > cfg.Alerts.StaleAfter:
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
