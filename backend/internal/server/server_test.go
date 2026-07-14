package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

const testToken = "testtoken"

func testServer() *Server {
	sum := sha256.Sum256([]byte(testToken))
	cfg := &config.Config{
		Provider: "google",
		Firmware: config.FirmwareConfig{Version: "1.1.0", URL: "https://x/fw-1.1.0.bin"},
		Rooms:    []config.Room{{DeviceID: "rt-1", Name: "Aspen", Room: "a@x", TokenSHA256: hex.EncodeToString(sum[:])}},
	}
	cfg.Wake.Timezone = "UTC"
	cfg.Alerts = config.AlertConfig{LowBatteryPct: 45, ClearPct: 55, MinRenotify: 24 * time.Hour, StaleAfter: time.Hour}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	alerts := notify.NewManager("", 45, 55, 24*time.Hour, log)
	return New(cfg, cache.New(), telemetry.New(), alerts, log)
}

// The exact JSON the firmware's tlm::to_json emits (incl. SHT4x env fields) must decode and
// surface as metrics — this guards the firmware↔backend contract.
func TestTelemetryToMetrics(t *testing.T) {
	srv := httptest.NewServer(testServer().Handler())
	defer srv.Close()

	body := `{"fw":"1.0.0","batt_mv":3470,"batt_pct":40,"heap_free":120000,"heap_min":90000,` +
		`"rssi":-58,"wake":"timer","wake_ms":1450,"rendered":false,"boot":42,"temp_c":22.4,"rh":47}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/telemetry/rt-1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("telemetry POST: err=%v code=%v", err, resp.StatusCode)
	}

	m, _ := http.Get(srv.URL + "/metrics")
	out, _ := io.ReadAll(m.Body)
	s := string(out)
	for _, want := range []string{
		`md_battery_percent{device="rt-1"}40`,
		`md_room_temp_celsius{device="rt-1"}22.4`,
		`md_room_humidity_percent{device="rt-1"}47`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestTelemetryRejectsBadToken(t *testing.T) {
	srv := httptest.NewServer(testServer().Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/telemetry/rt-1", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// A device that already shows the cached frame gets a 304 (no panel refresh) but still receives
// the OTA advertisement headers.
func TestDisplayServesOTAHeaders(t *testing.T) {
	s := testServer()
	s.cache.Set("rt-1", cache.Entry{ETag: `"abc123"`})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/display/rt-1", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("If-None-Match", `"abc123"`)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("want 304, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Fw-Target"); got != "1.1.0" {
		t.Fatalf("X-Fw-Target = %q, want 1.1.0", got)
	}
}

// The once-daily forced-refresh override bypasses a matching If-None-Match exactly once per
// device per day, so a room that never changes still gets a periodic real repaint (anti-ghosting).
func TestDisplayForcedDailyRefresh(t *testing.T) {
	s := testServer()
	hour := time.Now().UTC().Hour()
	s.cfg.Wake.ForcedRefreshHour = &hour
	s.cache.Set("rt-1", cache.Entry{ETag: `"abc123"`})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	get := func() *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/display/rt-1", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("If-None-Match", `"abc123"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET display: %v", err)
		}
		return resp
	}

	resp := get()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request in the forced hour: want 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Forced-Refresh") != "1" {
		t.Fatal("want X-Forced-Refresh: 1 on the forced repaint")
	}

	resp = get()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("second request same day: want 304 (already forced today), got %d", resp.StatusCode)
	}
}

// /api/v1/status and /status are unauthenticated (cluster-internal trust boundary, same as
// /metrics) and must reflect the same per-device classification.
func TestStatusEndpoints(t *testing.T) {
	s := testServer()
	s.tlm.Ingest("rt-1", telemetry.Report{BatteryPct: 20}, time.Now())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	jsonResp, err := http.Get(srv.URL + "/api/v1/status")
	if err != nil || jsonResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/status: err=%v code=%v", err, jsonResp.StatusCode)
	}
	body, _ := io.ReadAll(jsonResp.Body)
	if !strings.Contains(string(body), `"status":"low_battery"`) {
		t.Errorf("status JSON missing low_battery classification: %s", body)
	}

	page, err := http.Get(srv.URL + "/status")
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /status: err=%v code=%v", err, page.StatusCode)
	}
	htmlBody, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(htmlBody), "Aspen") {
		t.Errorf("status page missing room name: %s", htmlBody)
	}

	formatJSON, _ := http.Get(srv.URL + "/status?format=json")
	if ct := formatJSON.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("status?format=json Content-Type = %q, want application/json", ct)
	}
}
