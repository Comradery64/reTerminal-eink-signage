package server

import (
	"html/template"
	"net/http"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// receptionistRow is status-only — unlike managerRow, it carries no wake-mode fields, because
// this role has no controls at all, just the same read-only roster /manager shows.
type receptionistRow struct {
	status.Device
	BatteryBar string
}

func (s *Server) handleReceptionistPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	devices := status.Build(cfg, s.tlm, time.Now())

	rows := make([]receptionistRow, 0, len(devices))
	for _, d := range devices {
		rows = append(rows, receptionistRow{Device: d, BatteryBar: batteryBar(d.BatteryPct)})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := receptionistPageTmpl.Execute(w, rows); err != nil {
		s.log.Error("receptionist page render failed", "err", err)
	}
}

var receptionistPageTmpl = template.Must(template.New("receptionist").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Meeting display fleet — receptionist</title>
<style>` + baseCSS + `</style>
</head>
<body>
<div class="topbar">
<div class="masthead">` + brandMark + `<h1>Room status</h1></div>
<form method="POST" action="/receptionist/logout"><button type="submit" class="ghost">Log out</button></form>
</div>
<div class="grid">
{{range .}}
<div class="card surface">
<span class="chip chip-{{.Status}}">{{.Status}}</span>
<h2>{{.Name}}</h2>
<p class="id mono">{{.DeviceID}}</p>
<p class="readout"><span class="bar">{{.BatteryBar}}</span> {{.BatteryPct}}% &middot; seen {{.LastSeenSeconds}}s ago</p>
</div>
{{end}}
</div>
</body>
</html>
`))
