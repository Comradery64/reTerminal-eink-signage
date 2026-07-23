package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// dashboardPageView is the top-level template data: the room roster plus which nav links to show.
// Every role satisfies viewer (see auth.Role.Satisfies), so /dashboard is the one page every
// account lands on after login — ShowManager/ShowAdmin add the links into the higher-privilege
// panels an account may also hold, in place of requiring a separate login at each door.
type dashboardPageView struct {
	Rows        []dashboardRow
	ShowManager bool
	ShowAdmin   bool
}

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
	BatteryBar   template.HTML
	BatteryText  string // "78%", or "unknown" if this device has never reported — never blank, so
	StatusLabel  string // every card reserves the same space for this row and the grid stays even
	LastSeenText string
	NextCheckIn  string // e.g. "MON ~3:45 PM"; empty if unknown
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
		row := dashboardRow{Device: d, BatteryBar: batteryGauge(d.BatteryPct), StatusLabel: friendlyStatusLabel(d.Status)}
		if d.Status == "unreported" {
			row.LastSeenText = "unknown"
			row.BatteryText = "unknown"
		} else {
			row.LastSeenText = humanizeAgo(d.LastSeenSeconds)
			row.BatteryText = fmt.Sprintf("%d%%", d.BatteryPct)
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
			nextLocal := now.Add(time.Duration(secs) * time.Second).In(cfg.Location())
			row.NextCheckIn = strings.ToUpper(nextLocal.Format("Mon")) + " ~" + nextLocal.Format("3:04 PM")
		}

		rows = append(rows, row)
	}

	view := dashboardPageView{Rows: rows}
	// Re-derive the role from the same cookie requireRole(viewerUI, ...) already validated to
	// reach this handler at all — a fresh lookup here, rather than threading session state through
	// requireRole, mirrors how handleChangePasswordSubmit/handleTOTPVerifySubmit already look up
	// their own session independently.
	if c, err := r.Cookie(viewerUI.cookieName); err == nil {
		if sess, ok := s.sessions.Check(c.Value); ok {
			view.ShowManager = sess.Role.Satisfies(managerUI.role)
			view.ShowAdmin = sess.Role.Satisfies(adminUI.role)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardPageTmpl.Execute(w, view); err != nil {
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
.checkin { margin: 0 0 var(--space-3); font-family: var(--font-mono); font-size: var(--text-sm); color: var(--ink-soft); }
.checkin p { margin: 0; }
.checkin-value { color: var(--ink); }
</style>
</head>
<body>
<div class="topbar">
<div class="masthead">` + brandMark + `<h1>Room status</h1></div>
<div class="nav-links">
{{if .ShowAdmin}}<a href="/admin">Admin panel</a>{{end}}
{{if .ShowManager}}<a href="/manager">Manager panel</a>{{end}}
<form method="POST" action="/dashboard/logout"><button type="submit" class="ghost">Log out</button></form>
</div>
</div>
<div class="grid">
{{range .Rows}}
<div class="card surface">
<span class="chip chip-{{.Status}}">{{.StatusLabel}}</span>
<h2>{{.Name}}</h2>
<p class="readout">{{.BatteryBar}} {{.BatteryText}}</p>
<div class="checkin">
<p>last check-in: <span class="checkin-value">{{.LastSeenText}}</span></p>
{{if .NextCheckIn}}<p>next check-in: <span class="checkin-value">{{.NextCheckIn}}</span></p>{{end}}
</div>
<img src="/dashboard/preview/{{.DeviceID}}" alt="Last rendered display for {{.Name}}" loading="lazy">
</div>
{{end}}
</div>
</body>
</html>
`))
