package server

import (
	"html/template"
	"net/http"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// dashboardRow is status-only — unlike managerRow, it carries no wake-mode fields, because this
// role has no controls at all, just the same read-only roster /manager shows.
type dashboardRow struct {
	status.Device
	BatteryBar string
}

func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	devices := status.Build(cfg, s.tlm, time.Now())

	rows := make([]dashboardRow, 0, len(devices))
	for _, d := range devices {
		rows = append(rows, dashboardRow{Device: d, BatteryBar: batteryBar(d.BatteryPct)})
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
<span class="chip chip-{{.Status}}">{{.Status}}</span>
<h2>{{.Name}}</h2>
<p class="id mono">{{.DeviceID}}</p>
<p class="readout"><span class="bar">{{.BatteryBar}}</span> {{.BatteryPct}}% &middot; seen {{.LastSeenSeconds}}s ago</p>
<img src="/dashboard/preview/{{.DeviceID}}" alt="Last rendered display for {{.Name}}" loading="lazy">
</div>
{{end}}
</div>
</body>
</html>
`))
