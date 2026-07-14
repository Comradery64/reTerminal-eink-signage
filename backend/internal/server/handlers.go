package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

const maxTelemetryBody = 4 << 10 // 4 KiB is plenty for a health report

// handleDisplay is the hot path. It is allocation-light: it serves bytes straight from cache,
// and honors If-None-Match so an unchanged room costs the device zero panel-refresh energy.
func (s *Server) handleDisplay(w http.ResponseWriter, r *http.Request) {
	device := r.PathValue("device")
	if !s.auth.verify(device, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	now := time.Now()
	entry, ok := s.cache.Get(device)
	nextWake := s.cfg.NextWakeSeconds(now)
	if !ok || entry.ETag == "" {
		// Poller hasn't produced a frame yet (cold start) or the room is failing.
		setNoWake(w, nextWake)
		// Tell the device to retry soon rather than nap a full interval on cold start.
		w.Header().Set("Retry-After", "30")
		http.Error(w, "no payload yet", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("ETag", entry.ETag)
	setNoWake(w, entry.Payload.NextWakeS)
	s.setFirmwareHeaders(w) // OTA advertisement (sent on both 200 and 304)

	// Once-daily anti-ghosting override: force a real repaint even though content is unchanged,
	// so a room that never changes doesn't sit on the same frame indefinitely. Opt-in via
	// wake.forced_refresh_hour; disabled (nil) by default.
	forceRefresh := false
	if h := s.cfg.Wake.ForcedRefreshHour; h != nil {
		forceRefresh = s.cache.ShouldForceFullRefresh(device, now, *h, s.cfg.Location())
	}

	// Conditional GET: identical content → 304, device skips the panel refresh entirely.
	if match := r.Header.Get("If-None-Match"); !forceRefresh && match != "" && match == entry.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if forceRefresh {
		w.Header().Set("X-Forced-Refresh", "1") // observability only; firmware ignores it
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Explicit Content-Length required: calling WriteHeader before Write prevents Go from setting
	// it automatically, causing chunked encoding. The ESP32 firmware parser requires Content-Length.
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.Payload.Bytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(entry.Payload.Bytes)
}

// setFirmwareHeaders advertises the OTA target to devices. No-op if no firmware version is set.
func (s *Server) setFirmwareHeaders(w http.ResponseWriter) {
	if s.cfg.Firmware.Version == "" {
		return
	}
	w.Header().Set("X-Fw-Target", s.cfg.Firmware.Version)
	if s.cfg.Firmware.URL != "" {
		w.Header().Set("X-Fw-Url", s.cfg.Firmware.URL)
	}
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	device := r.PathValue("device")
	if !s.auth.verify(device, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var rep telemetry.Report
	dec := json.NewDecoder(io.LimitReader(r.Body, maxTelemetryBody))
	if err := dec.Decode(&rep); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	now := time.Now()
	s.tlm.Ingest(device, rep, now)
	// Hysteresis + dedupe + dispatch all live in the alert manager; this is non-blocking.
	s.alerts.EvaluateBattery(device, s.names[device], rep.BatteryPct, rep.BatteryMV, now)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.tlm.WriteMetrics(w, time.Now())
}
