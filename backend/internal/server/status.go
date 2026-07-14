package server

import (
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// handleStatusJSON serves GET /api/v1/status — the machine-readable fleet health view.
// Unauthenticated by design: same cluster-internal-only trust boundary as /metrics (see
// docs/DASHBOARD.md Decisions) — never add this path to the public Ingress.
func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status.Build(s.cfg, s.tlm, time.Now()))
}

// handleStatusPage serves GET /status — a minimal server-rendered HTML table over the same
// data, for a building manager without kubectl/API access. ?format=json returns the JSON body
// instead, so the same path also works for scripted polling.
func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") == "json" {
		s.handleStatusJSON(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusPageTmpl.Execute(w, status.Build(s.cfg, s.tlm, time.Now())); err != nil {
		s.log.Error("status page render failed", "err", err)
	}
}

var statusPageTmpl = template.Must(template.New("status").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Meeting display fleet status</title>
<style>
body { font-family: system-ui, sans-serif; margin: 2rem; color: #1a1a1a; }
table { border-collapse: collapse; width: 100%; }
th, td { border: 1px solid #ddd; padding: .5rem .75rem; text-align: left; }
th { background: #f5f5f5; }
.status-ok { color: #1a7f37; }
.status-stale, .status-low_battery { color: #9a6700; }
.status-unreported { color: #cf222e; }
</style>
</head>
<body>
<h1>Meeting display fleet status</h1>
<table>
<tr><th>Room</th><th>Device</th><th>Status</th><th>Battery</th><th>Last seen</th><th>Last refresh</th><th>Signal</th><th>Firmware</th><th>Boots</th></tr>
{{range .}}
<tr>
<td>{{.Name}}</td>
<td>{{.DeviceID}}</td>
<td class="status-{{.Status}}">{{.Status}}</td>
<td>{{.BatteryPct}}% ({{.BatteryMV}} mV)</td>
<td>{{.LastSeenSeconds}}s ago</td>
<td>{{.LastRenderDisplay}}</td>
<td>{{.RSSI}} dBm</td>
<td>{{.FirmwareVersion}}</td>
<td>{{.BootCount}}</td>
</tr>
{{end}}
</table>
</body>
</html>
`))
