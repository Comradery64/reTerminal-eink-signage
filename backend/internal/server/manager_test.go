package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func loggedInManagerClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := srv.Client()
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.PostForm(srv.URL+"/manager/login", url.Values{"username": {testManagerUsername}, "password": {testManagerPassword}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("manager login: want 303, got %d", resp.StatusCode)
	}
	return client
}

func TestManagerPageRequiresLogin(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(srv.URL + "/manager")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect to login, got %d", resp.StatusCode)
	}
}

func TestManagerPageListsRoomAndSavesWakeOverride(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInManagerClient(t, srv)

	page, err := client.Get(srv.URL + "/manager")
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /manager: err=%v code=%v", err, page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), "Aspen") {
		t.Errorf("manager page missing room name: %s", body)
	}

	resp, err := client.PostForm(srv.URL+"/manager/wake/save", url.Values{
		"device_id":        {"rt-1"},
		"wake_mode":        {"flat"},
		"interval_seconds": {"600"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("wake save: want 303, got %d", resp.StatusCode)
	}

	updated := s.cfg.Load()
	room, ok := updated.RoomByDeviceID("rt-1")
	if !ok || room.WakeMode == nil || *room.WakeMode != "flat" || room.FlatIntervalSeconds == nil || *room.FlatIntervalSeconds != 600 {
		t.Fatalf("wake override did not apply: %+v", room)
	}
}

func TestManagerSaveWakeRejectsUnknownDevice(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInManagerClient(t, srv)

	before := s.cfg.Load()
	resp, err := client.PostForm(srv.URL+"/manager/wake/save", url.Values{
		"device_id": {"does-not-exist"},
		"wake_mode": {"smart"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect back with error, got %d", resp.StatusCode)
	}
	if s.cfg.Load() != before {
		t.Fatal("a rejected write must not swap the live config")
	}
}

// TestManagerSaveWakePersistFailureCarriesRealReason guards against collapsing a durable-backend
// persist failure (e.g. a 403'ing ConfigMap PATCH) into the same generic "?error=rejected" text
// used for an ordinary validation rejection above — the two are indistinguishable to a building
// manager, who will "try again" forever on a failure retrying can never fix, and the actual
// actionable persistFailureMessage would otherwise be discarded.
func TestManagerSaveWakePersistFailureCarriesRealReason(t *testing.T) {
	s := testServerWithAuth(t)
	s.persist = failingPersistStore{err: errors.New("403 Forbidden")}
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInManagerClient(t, srv)

	resp, err := client.PostForm(srv.URL+"/manager/wake/save", url.Values{
		"device_id":        {"rt-1"},
		"wake_mode":        {"flat"},
		"interval_seconds": {"600"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/manager?error=persist&msg=") {
		t.Fatalf("want a persist-failure redirect distinct from ?error=rejected, got %q", loc)
	}
	if !strings.Contains(loc, "403+Forbidden") && !strings.Contains(loc, "403%2BForbidden") && !strings.Contains(loc, "403") {
		t.Fatalf("redirect must carry the real failure reason, got %q", loc)
	}

	// Following the redirect must render the real reason in the page's error banner, not the
	// generic "try again" text used for a plain validation rejection.
	page, err := client.Get(srv.URL + loc)
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: err=%v code=%v", loc, err, page)
	}
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), "403 Forbidden") {
		t.Fatalf("manager page must show the real persist failure reason, got:\n%s", body)
	}
	if strings.Contains(string(body), "Try again, or ask IT if it keeps failing.") {
		t.Fatalf("manager page must not show the generic rejected-validation text for a persist failure")
	}
}
