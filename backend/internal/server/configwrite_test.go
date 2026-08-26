package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// failingPersistStore simulates a durable backend (e.g. a 403'ing ConfigMap PATCH) whose Persist
// always fails — used to prove applyConfig persists BEFORE swapping the live config.
type failingPersistStore struct{ err error }

func (f failingPersistStore) Persist(context.Context, []byte) error { return f.err }
func (f failingPersistStore) Mode() string                          { return "configmap" }
func (f failingPersistStore) Target() string                        { return "meeting-displays/broker-config" }
func (f failingPersistStore) Durable() bool                         { return true }

// noneStore simulates config_persistence.mode=none — never durable, Persist is a no-op.
type noneStore struct{}

func (noneStore) Persist(context.Context, []byte) error { return nil }
func (noneStore) Mode() string                          { return "none" }
func (noneStore) Target() string                        { return "" }
func (noneStore) Durable() bool                         { return false }

func TestApplyConfigPersistsBeforeSwappingLiveConfig(t *testing.T) {
	s := testServerWithAuth(t)
	s.persist = failingPersistStore{err: errors.New("403 Forbidden")}

	before := s.cfg.Load()
	newCfg, err := before.WithFirmware(config.FirmwareConfig{Version: "9.9.9"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.applyConfig(context.Background(), newCfg, false); err == nil {
		t.Fatal("want an error when the durable persister fails")
	}
	if s.cfg.Load() != before {
		t.Fatal("a failed persist must leave the live config unchanged — fail-closed, not 'saved' followed by silent reversion")
	}
}

func TestApplyConfigSwapsOnlyAfterSuccessfulPersist(t *testing.T) {
	s := testServerWithAuth(t)
	newCfg, err := s.cfg.Load().WithFirmware(config.FirmwareConfig{Version: "9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyConfig(context.Background(), newCfg, false); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}
	if s.cfg.Load().Firmware.Version != "9.9.9" {
		t.Fatal("a successful persist must swap the live config")
	}
}

func TestApplyConfigSkipsPersistStepWhenNotDurable(t *testing.T) {
	s := testServerWithAuth(t)
	s.persist = noneStore{}
	newCfg, err := s.cfg.Load().WithFirmware(config.FirmwareConfig{Version: "8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyConfig(context.Background(), newCfg, false); err != nil {
		t.Fatalf("applyConfig with a non-durable store must never fail: %v", err)
	}
	if s.cfg.Load().Firmware.Version != "8.8.8" {
		t.Fatal("a non-durable store must still apply the change in memory (ephemeral, not rejected)")
	}
}

func TestAdminSaveRedirectsToErrorOnPersistFailureAndSavedOnSuccess(t *testing.T) {
	s := testServerWithAuth(t)
	s.persist = failingPersistStore{err: errors.New("403 Forbidden")}
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.PostForm(srv.URL+"/admin/firmware/save", url.Values{"version": {"3.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/admin?error=") {
		t.Fatalf("want redirect to an error banner, got %q", loc)
	}
	if s.cfg.Load().Firmware.Version == "3.0.0" {
		t.Fatal("a failed persist must not have changed the live config")
	}

	// Same handler, healthy persister: must redirect to the ordinary saved= flash.
	s.persist = noopPersistStore{}
	resp, err = client.PostForm(srv.URL+"/admin/firmware/save", url.Values{"version": {"3.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Location"); got != "/admin?saved=firmware" {
		t.Fatalf("want /admin?saved=firmware, got %q", got)
	}
	if s.cfg.Load().Firmware.Version != "3.0.0" {
		t.Fatal("a successful save must have applied")
	}
}

func TestPersistStripRendersHealthyNoneAndErrorStates(t *testing.T) {
	s := testServerWithAuth(t)

	s.pstate = &persistState{}
	s.pstate.recordOK("file", "/etc/meeting-displays/config.yaml", true, time.Now())
	strip := s.persistStrip()
	if strip.Class != "" || !strings.Contains(strip.Message, "/etc/meeting-displays/config.yaml") {
		t.Fatalf("healthy strip = %+v", strip)
	}

	s.pstate = &persistState{}
	s.pstate.recordOK("none", "", false, time.Now())
	strip = s.persistStrip()
	if strip.Class != "banner-warn" || !strings.Contains(strip.Message, "LOST when the broker restarts") {
		t.Fatalf("none strip = %+v", strip)
	}

	s.pstate = &persistState{}
	s.pstate.recordErr("configmap", "meeting-displays/broker-config", true, errors.New("403 Forbidden"), time.Now())
	strip = s.persistStrip()
	if strip.Class != "banner-error" || !strings.Contains(strip.Message, "403 Forbidden") {
		t.Fatalf("error strip = %+v", strip)
	}
}

// TestConfigPersistenceAPIReflectsState proves the exact thing docs/DEPLOY-TIERS.md,
// backend/deploy/systemd/README.md and backend/deploy/compose/README.md all document: a plain,
// unauthenticated `curl localhost:8080/api/v1/config-persistence` — no cookie jar, no login —
// returns the persistence JSON. This endpoint deliberately sits in the same unauthenticated,
// internal-only trust boundary as /metrics and /api/v1/status (see server.go's Handler): it used
// to be requireRole(adminUI, ...)-gated, but admin_session is scoped to Path=/admin (see
// setSessionCookies), so per RFC 6265 no browser or curl cookie jar would ever attach it to a
// request under /api/v1/... — every real call site in the docs got a 303-to-login redirect
// instead of JSON, so a monitoring probe built on it would never observe durable:false and would
// silently never alert.
func TestConfigPersistenceAPIReflectsState(t *testing.T) {
	s := testServerWithAuth(t)
	s.pstate.recordErr("configmap", "meeting-displays/broker-config", true, errors.New("403 Forbidden"), time.Now())
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := srv.Client()

	resp, err := client.Get(srv.URL + "/api/v1/config-persistence")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("unauthenticated GET /api/v1/config-persistence: err=%v code=%v", err, resp)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (docs show curl parsing JSON, not following an HTML login redirect)", ct)
	}
	var got struct {
		Mode      string `json:"mode"`
		Target    string `json:"target"`
		Durable   bool   `json:"durable"`
		LastError string `json:"last_error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "configmap" || got.Target != "meeting-displays/broker-config" || !got.Durable {
		t.Fatalf("unexpected response: %+v", got)
	}
	if !strings.Contains(got.LastError, "403 Forbidden") {
		t.Fatalf("last_error = %q, want it to contain the recorded failure", got.LastError)
	}
}
