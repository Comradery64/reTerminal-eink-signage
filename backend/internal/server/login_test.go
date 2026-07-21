package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

const (
	testAdminUsername        = "admin"
	testAdminPassword        = "admin-secret"
	testManagerUsername      = "manager"
	testManagerPassword      = "manager-secret"
	testReceptionistUsername = "frontdesk"
	testReceptionistPassword = "front-desk-secret"
)

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// testServerWithAuth builds a *Server with one admin, one manager, and one receptionist account
// configured, for exercising the login/logout/requireRole flow.
func testServerWithAuth(t *testing.T) *Server {
	t.Helper()
	sum := sha256.Sum256([]byte(testToken))
	cfg := &config.Config{
		Provider: "google",
		Rooms:    []config.Room{{DeviceID: "rt-1", Name: "Aspen", Room: "a@x", TokenSHA256: hex.EncodeToString(sum[:])}},
		Users: []config.User{
			{Username: testAdminUsername, PasswordSHA256: hashHex(testAdminPassword), Role: "admin"},
			{Username: testManagerUsername, PasswordSHA256: hashHex(testManagerPassword), Role: "manager"},
			{Username: testReceptionistUsername, PasswordSHA256: hashHex(testReceptionistPassword), Role: "receptionist"},
		},
		Auth: config.AuthConfig{SessionSecret: "01234567890123456789012345678901"},
	}
	cfg.Wake.Timezone = "UTC"
	cfg.Alerts = config.AlertConfig{LowBatteryPct: 45, ClearPct: 55, MinRenotify: 24 * time.Hour, StaleAfter: time.Hour}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	alerts := notify.NewManager("", 45, 55, 24*time.Hour, log)
	return New(config.NewLive(cfg), cache.New(), telemetry.New(), alerts, log)
}

func TestRequireRoleRedirectsWithoutCookie(t *testing.T) {
	s := testServerWithAuth(t)
	s.Handler() // registers login routes; the mux itself isn't needed for this unit check
	handlerCalled := false
	guarded := s.requireRole(adminUI, func(w http.ResponseWriter, r *http.Request) { handlerCalled = true })

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	guarded(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 redirect, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Fatal("want a Location header pointing at the login page")
	}
	if handlerCalled {
		t.Fatal("guarded handler must not run without a valid session")
	}
}

func TestLoginFlowEndToEnd(t *testing.T) {
	s := testServerWithAuth(t)
	mux := s.Handler()

	var gotRole string
	// Register a probe behind requireRole to observe whether the guarded handler actually runs.
	probe := http.NewServeMux()
	probe.Handle("/", mux)
	// Cookie is scoped to Path: adminUI.homePath ("/admin"), so the probe must live under that
	// path too or the browser (and cookiejar) will never attach it — mirrors how a real
	// requireRole-guarded /admin/* route works in production.
	probe.HandleFunc("GET /admin/probe", s.requireRole(adminUI, func(w http.ResponseWriter, r *http.Request) {
		gotRole = "admin"
		w.WriteHeader(http.StatusOK)
	}))

	// Login sets Secure cookies (this broker always sits behind TLS in production, see login.go),
	// so the test server must actually speak TLS for the cookie jar to retain and resend them.
	srv := httptest.NewTLSServer(probe)
	defer srv.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := srv.Client()
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// Wrong password: redirected back to login with an error, no cookie set.
	resp, err := client.PostForm(srv.URL+"/admin/login", url.Values{"username": {testAdminUsername}, "password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("wrong password: want 303, got %d", resp.StatusCode)
	}

	// Correct password: sets a cookie the probe route accepts.
	resp, err = client.PostForm(srv.URL+"/admin/login", url.Values{"username": {testAdminUsername}, "password": {testAdminPassword}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct password: want 303, got %d", resp.StatusCode)
	}

	probeResp, err := client.Get(srv.URL + "/admin/probe")
	if err != nil {
		t.Fatal(err)
	}
	if probeResp.StatusCode != http.StatusOK || gotRole != "admin" {
		t.Fatalf("guarded probe after login: status=%d gotRole=%q", probeResp.StatusCode, gotRole)
	}

	// Logout clears the session; the probe must reject again.
	if _, err := client.Post(srv.URL+"/admin/logout", "", nil); err != nil {
		t.Fatal(err)
	}
	gotRole = ""
	probeResp, err = client.Get(srv.URL + "/admin/probe")
	if err != nil {
		t.Fatal(err)
	}
	if probeResp.StatusCode == http.StatusOK {
		t.Fatal("probe must reject after logout")
	}
}

func TestManagerLoginDoesNotGrantAdmin(t *testing.T) {
	s := testServerWithAuth(t)
	guarded := s.requireRole(adminUI, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	token := s.sessions.Create(managerUI.role)
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: adminUI.cookieName, Value: token})
	rec := httptest.NewRecorder()
	guarded(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a manager session presented as admin_session must be rejected, got %d", rec.Code)
	}
}
