package server

import (
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
