package server

import (
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/status"
)

// managerRow is one room's status.Device (read-only, reused from /status) plus the one control
// the manager UI exposes: wake mode. WakeMode/FlatIntervalSeconds reflect the room's *current*
// override if set, else the fleet default (empty WakeMode / 0 interval), so the form pre-fills
// with what's actually in effect rather than always defaulting to "smart".
type managerRow struct {
	status.Device
	WakeMode            string // "flat" or "smart" — resolved effective value, never empty
	FlatIntervalSeconds uint32 // 0 if this room has no override and the fleet default applies
	BatteryBar          string
}

type managerPageData struct {
	Rows      []managerRow
	SavedRoom string // device_id just saved, for the one-time flash — empty if this was a plain load
	ErrorMsg  string // pre-written for the reader, never a raw Go error (see handleManagerSaveWake)
}

func (s *Server) handleManagerPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	devices := status.Build(cfg, s.tlm, time.Now())

	rows := make([]managerRow, 0, len(devices))
	for _, d := range devices {
		room, _ := cfg.RoomByDeviceID(d.DeviceID)
		mode := cfg.Wake.Mode
		if mode == "" {
			mode = "flat"
		}
		if room.WakeMode != nil && *room.WakeMode != "" {
			mode = *room.WakeMode
		}
		var interval uint32
		if room.FlatIntervalSeconds != nil {
			interval = *room.FlatIntervalSeconds
		}
		rows = append(rows, managerRow{
			Device: d, WakeMode: mode, FlatIntervalSeconds: interval,
			BatteryBar: batteryBar(d.BatteryPct),
		})
	}

	data := managerPageData{Rows: rows, SavedRoom: r.URL.Query().Get("saved")}
	switch r.URL.Query().Get("error") {
	case "invalid_interval":
		data.ErrorMsg = "Enter a whole number of minutes for Simple mode — that value didn't save."
	case "rejected":
		data.ErrorMsg = "That change didn't save. Try again, or ask IT if it keeps failing."
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := managerPageTmpl.Execute(w, data); err != nil {
		s.log.Error("manager page render failed", "err", err)
	}
}

func (s *Server) handleManagerSaveWake(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	deviceID := r.PostForm.Get("device_id")
	mode := r.PostForm.Get("wake_mode")

	var flatSeconds *uint32
	if mode == "flat" {
		secs, err := strconv.ParseUint(r.PostForm.Get("interval_seconds"), 10, 32)
		if err != nil || secs == 0 {
			http.Redirect(w, r, "/manager?error=invalid_interval", http.StatusSeeOther)
			return
		}
		v := uint32(secs)
		flatSeconds = &v
	}

	newCfg, err := s.cfg.Load().WithRoomWakeOverride(deviceID, &mode, flatSeconds)
	if err != nil {
		s.log.Error("manager wake save rejected", "device", deviceID, "err", err)
		http.Redirect(w, r, "/manager?error=rejected", http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, false) // wake override never touches tokens/names/credentials
	http.Redirect(w, r, "/manager?saved="+template.URLQueryEscaper(deviceID), http.StatusSeeOther)
}

var managerPageTmpl = template.Must(template.New("manager").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Meeting display fleet — manager</title>
<style>` + baseCSS + `
/* Tab-style underline, not a filled block: a solid fill for the selected side reads as visually
   heavier/bigger than its box even when the two halves are pixel-equal (a real optical illusion),
   and a full block was also just visual clutter for a two-option control. An underline is thin
   enough not to trigger that illusion and reads clean at this size. */
.segmented { display: flex; border-bottom: 1px solid var(--line); margin-bottom: var(--space-3); }
.segmented input { position: absolute; opacity: 0; pointer-events: none; }
.segmented label {
  flex: 1; min-height: var(--control-h); display: flex; align-items: center; justify-content: center;
  font-size: var(--text-sm); font-weight: 600; text-transform: uppercase; letter-spacing: .03em;
  cursor: pointer; color: var(--ink-soft); text-align: center;
  border-bottom: 2px solid transparent; margin-bottom: -1px;
}
.segmented input:checked + label { color: var(--ink); border-bottom-color: var(--ink); }
/* interval-wrap is a SIBLING of .segmented (not nested inside it) specifically so showing the
   number field only grows the form, never the toggle's own box — nesting it inside .segmented
   made the toggle 8px taller in Simple mode than in Smart mode, a real layout bug caught in
   review. :has() reaches across that sibling boundary; :checked ~ alone couldn't. */
.wake-form:has(.simple-toggle:checked) .interval-wrap { display: block; }
.interval-wrap { display: none; margin: var(--space-3) 0; }
.interval-wrap input { width: 100%; }
.wake-form button[type=submit] { width: 100%; margin-top: 0; }
</style>
</head>
<body>
<div class="topbar">
<div class="masthead">` + brandMark + `<h1>Room status</h1></div>
<div class="nav-links">
<a href="/dashboard">Dashboard</a>
<form method="POST" action="/manager/logout"><button type="submit" class="ghost">Log out</button></form>
</div>
</div>
{{if .ErrorMsg}}<p class="banner banner-error">{{.ErrorMsg}}</p>{{end}}
<div class="grid">
{{range .Rows}}
<div class="card surface {{if eq $.SavedRoom .DeviceID}}flash{{end}}" data-device="{{.DeviceID}}">
<span class="chip chip-{{.Status}}">{{.Status}}</span>
<h2>{{.Name}}</h2>
<p class="id mono">{{.DeviceID}}</p>
<p class="readout"><span class="bar">{{.BatteryBar}}</span> {{.BatteryPct}}% &middot; seen {{.LastSeenSeconds}}s ago</p>
<form class="wake-form" method="POST" action="/manager/wake/save">
<input type="hidden" name="device_id" value="{{.DeviceID}}">
<div class="segmented">
<input type="radio" name="wake_mode" value="smart" id="mode-{{.DeviceID}}-smart" {{if eq .WakeMode "smart"}}checked{{end}}>
<label for="mode-{{.DeviceID}}-smart">Smart</label>
<input class="simple-toggle" type="radio" name="wake_mode" value="flat" id="mode-{{.DeviceID}}-flat" {{if eq .WakeMode "flat"}}checked{{end}}>
<label for="mode-{{.DeviceID}}-flat">Simple</label>
</div>
<div class="interval-wrap">
<input type="number" name="interval_seconds" min="60" step="60" value="{{.FlatIntervalSeconds}}" placeholder="Seconds between checks">
</div>
<button type="submit">Save</button>
</form>
</div>
{{end}}
</div>
</body>
</html>
`))
