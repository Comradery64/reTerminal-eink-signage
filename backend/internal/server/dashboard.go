package server

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// dashboardRow is status-only — unlike managerRow, it carries no wake-mode fields, because this
// role has no controls at all, just the same read-only roster /manager shows.
//
// StatusLabel/LastSeenText/NextCheckIn exist because the raw status.Device fields (the terse
// "unreported" keyword, a raw seconds count) read as alarming or unclear to a non-technical
// viewer (e.g. a receptionist) even when they describe a perfectly normal state — a device on
// "smart" wake mode, or one that simply hasn't had its first check-in yet since a broker
// restart, looks identical to a genuinely broken device without this context.
type dashboardRow struct {
	status.Device
	BatteryBar   string
	StatusLabel  string
	LastSeenText string
	NextCheckIn  string // formatted local clock time, e.g. "3:45 PM"; empty if the room is unknown
}

// friendlyStatusLabel rephrases the internal status keyword (shared with /status and
// /api/v1/status, so not itself changed) into wording that doesn't read as an outage to someone
// without broker context.
func friendlyStatusLabel(s string) string {
	switch s {
	case "unreported":
		return "waiting"
	case "stale":
		return "overdue"
	case "low_battery":
		return "low battery"
	default:
		return "ok"
	}
}

// humanizeAgo turns a raw seconds-ago count into a coarse, readable duration.
func humanizeAgo(secs int64) string {
	switch {
	case secs < 60:
		return "just now"
	case secs < 3600:
		return fmt.Sprintf("%dm ago", secs/60)
	case secs < 86400:
		return fmt.Sprintf("%dh ago", secs/3600)
	default:
		return fmt.Sprintf("%dd ago", secs/86400)
	}
}

func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	now := time.Now()
	devices := status.Build(cfg, s.tlm, now)

	rows := make([]dashboardRow, 0, len(devices))
	for _, d := range devices {
		row := dashboardRow{Device: d, BatteryBar: batteryBar(d.BatteryPct), StatusLabel: friendlyStatusLabel(d.Status)}
		if d.Status == "unreported" {
			row.LastSeenText = "hasn't checked in yet"
		} else {
			row.LastSeenText = humanizeAgo(d.LastSeenSeconds)
		}

		// Next check-in uses the exact same calendar-aware calculation the device itself is told
		// to obey (config.NextWakeDuration, also used to set the device's X-Next-Wake header), so
		// this never drifts from reality — a room that hasn't reported yet still gets an honest
		// estimate instead of looking stuck.
		if room, ok := cfg.RoomByDeviceID(d.DeviceID); ok {
			var cur, next *calendar.Event
			if entry, ok := s.cache.Get(d.DeviceID); ok {
				cur, next = entry.Cur, entry.Next
			}
			secs := cfg.NextWakeDuration(room, cur, next, now)
			row.NextCheckIn = now.Add(time.Duration(secs) * time.Second).In(cfg.Location()).Format("3:04 PM")
		}

		rows = append(rows, row)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardPageTmpl.Execute(w, rows); err != nil {
		s.log.Error("dashboard page render failed", "err", err)
	}
}

// handleDashboardPreview serves the last rendered display image for one device as a PNG, so a
// viewer can see exactly what's currently on the e-ink panel without walking to the room. Not the
// bytes the device itself receives (those are the packed Spectra-6 payload) — this is a plain PNG
// encoding of the same composed image, cached alongside it since the last poll.
func (s *Server) handleDashboardPreview(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("device_id")
	entry, ok := s.cache.Get(deviceID)
	if !ok || len(entry.PreviewPNG) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(entry.PreviewPNG)
}

var dashboardPageTmpl = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Meeting display fleet — dashboard</title>
<style>` + baseCSS + `
.card img { display: block; width: 100%; margin-top: var(--space-3); border: 1px solid var(--line); border-radius: var(--radius); }
/* Last/next check-in as a small aligned label:value list, not one run-on line — "last check-in:
   hasn't checked in yet · next check-in ~12:20 PM" reads fine as prose but is genuinely harder to
   scan at a glance than two short stacked rows, which is the whole point of this card for a
   receptionist walking by. */
.checkin { display: grid; grid-template-columns: auto 1fr; column-gap: var(--space-2); row-gap: 2px; margin: 0 0 var(--space-3); }
.checkin dt, .checkin dd { font-family: var(--font-mono); font-size: var(--text-sm); color: var(--ink-soft); margin: 0; }
.checkin dt { text-align: right; }
</style>
</head>
<body>
<div class="topbar">
<div class="masthead">` + brandMark + `<h1>Room status</h1></div>
<form method="POST" action="/dashboard/logout"><button type="submit" class="ghost">Log out</button></form>
</div>
<div class="grid">
{{range .}}
<div class="card surface">
<span class="chip chip-{{.Status}}">{{.StatusLabel}}</span>
<h2>{{.Name}}</h2>
<p class="id mono">{{.DeviceID}}</p>
{{if ne .Status "unreported"}}<p class="readout"><span class="bar">{{.BatteryBar}}</span> {{.BatteryPct}}%</p>{{end}}
<dl class="checkin">
<dt>last check-in</dt><dd>{{.LastSeenText}}</dd>
{{if .NextCheckIn}}<dt>next check-in</dt><dd>~{{.NextCheckIn}}</dd>{{end}}
</dl>
<img src="/dashboard/preview/{{.DeviceID}}" alt="Last rendered display for {{.Name}}" loading="lazy">
</div>
{{end}}
</div>
</body>
</html>
`))
