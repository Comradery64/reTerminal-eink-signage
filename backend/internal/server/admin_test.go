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

func loggedInAdminClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := srv.Client()
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.PostForm(srv.URL+"/admin/login", url.Values{"username": {testAdminUsername}, "password": {testAdminPassword}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin login: want 303, got %d", resp.StatusCode)
	}
	return client
}

func TestAdminPageRequiresLogin(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(srv.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect to login, got %d", resp.StatusCode)
	}
}

func TestAdminPageListsRoomsAndMasksToken(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.Get(srv.URL + "/admin")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin: err=%v code=%v", err, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Aspen") {
		t.Errorf("admin page missing room name: %s", body)
	}
	if strings.Contains(string(body), s.cfg.Load().Rooms[0].TokenSHA256) {
		t.Fatal("admin page must never render the raw token hash")
	}
}

func TestAdminAddRoomThenLoginWithNewToken(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.PostForm(srv.URL+"/admin/rooms/save", url.Values{
		"device_id": {"rt-2"},
		"name":      {"Birch"},
		"room":      {"b@x"},
		"token":     {"new-device-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add room: want 303, got %d", resp.StatusCode)
	}

	cfg := s.cfg.Load()
	room, ok := cfg.RoomByDeviceID("rt-2")
	if !ok || room.Name != "Birch" {
		t.Fatalf("new room not present: %+v", cfg.Rooms)
	}

	// The derived device-auth table must have been rebuilt immediately (refreshCaches=true), so
	// the new device can authenticate to /api/v1/display without a restart.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/display/rt-2", nil)
	req.Header.Set("Authorization", "Bearer new-device-token")
	displayResp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if displayResp.StatusCode == http.StatusUnauthorized {
		t.Fatal("newly added room's token must authenticate immediately, no restart required")
	}
}

func TestAdminDeleteRoomRejectsLastRoom(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.PostForm(srv.URL+"/admin/rooms/delete", url.Values{"device_id": {"rt-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect, got %d", resp.StatusCode)
	}
	if _, ok := s.cfg.Load().RoomByDeviceID("rt-1"); !ok {
		t.Fatal("deleting the last room must be rejected, room should still exist")
	}
}

func TestAdminSaveWakeDefaultsAndAlertsAndFirmware(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	if resp, err := client.PostForm(srv.URL+"/admin/wake/save", url.Values{
		"mode": {"smart"}, "timezone": {"America/New_York"},
		"business_hours_seconds": {"900"}, "off_hours_seconds": {"3600"},
		"business_start_hour": {"8"}, "business_end_hour": {"18"},
	}); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("wake save: err=%v code=%v", err, resp)
	}
	if got := s.cfg.Load().Wake.Mode; got != "smart" {
		t.Fatalf("wake.mode = %q, want smart", got)
	}

	if resp, err := client.PostForm(srv.URL+"/admin/alerts/save", url.Values{
		"low_battery_pct": {"30"}, "clear_pct": {"50"},
		"min_renotify": {"12h"}, "stale_after": {"2h"},
	}); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("alerts save: err=%v code=%v", err, resp)
	}
	if got := s.cfg.Load().Alerts.LowBatteryPct; got != 30 {
		t.Fatalf("alerts.low_battery_pct = %d, want 30", got)
	}

	if resp, err := client.PostForm(srv.URL+"/admin/firmware/save", url.Values{
		"version": {"2.0.0"}, "url": {"https://x/fw-2.0.0.bin"},
	}); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("firmware save: err=%v code=%v", err, resp)
	}
	if got := s.cfg.Load().Firmware.Version; got != "2.0.0" {
		t.Fatalf("firmware.version = %q, want 2.0.0", got)
	}
}

func TestAdminSaveWakeDefaultsRejectsInvalidTimezone(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	before := s.cfg.Load()
	resp, err := client.PostForm(srv.URL+"/admin/wake/save", url.Values{"timezone": {"not-a-zone"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect back with error, got %d", resp.StatusCode)
	}
	if s.cfg.Load() != before {
		t.Fatal("a rejected wake-defaults write must not swap the live config")
	}
}

func TestAdminAccessPageListsUsersWithoutExposingPasswordHash(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.Get(srv.URL + "/admin")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin: err=%v code=%v", err, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, testManagerUsername) || !strings.Contains(html, testReceptionistUsername) {
		t.Errorf("admin page missing seeded usernames: %s", html)
	}
	for _, u := range s.cfg.Load().Users {
		if strings.Contains(html, u.PasswordSHA256) {
			t.Fatalf("admin page must never render a raw password hash (leaked for %q)", u.Username)
		}
	}
}

func TestAdminGrantAccessAddsUserWhoCanImmediatelyLogIn(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	adminClient := loggedInAdminClient(t, srv)

	resp, err := adminClient.PostForm(srv.URL+"/admin/access/save", url.Values{
		"username": {"newmanager"}, "password": {"new-pw"}, "role": {"manager"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("grant access: want 303, got %d", resp.StatusCode)
	}

	if _, ok := s.cfg.Load().UserByUsername("newmanager"); !ok {
		t.Fatal("new user not present in config")
	}

	// The directory must be rebuilt immediately (refreshCaches=true), so the new account can log
	// in without a restart.
	jar, _ := cookiejar.New(nil)
	client := srv.Client()
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	loginResp, err := client.PostForm(srv.URL+"/manager/login", url.Values{"username": {"newmanager"}, "password": {"new-pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("new manager login: want 303, got %d", loginResp.StatusCode)
	}
	page, err := client.Get(srv.URL + "/manager")
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /manager as newly granted user: err=%v code=%v", err, page)
	}
}

func TestAdminEditAccessChangesRoleWithoutChangingPassword(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.PostForm(srv.URL+"/admin/access/save", url.Values{
		"original_username": {testManagerUsername}, "username": {testManagerUsername}, "role": {"receptionist"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit access: want 303, got %d", resp.StatusCode)
	}

	updated, ok := s.cfg.Load().UserByUsername(testManagerUsername)
	if !ok || updated.Role != "receptionist" {
		t.Fatalf("role change did not apply: %+v", updated)
	}
	if updated.PasswordSHA256 != hashHex(testManagerPassword) {
		t.Fatal("leaving the password field blank must keep the existing password hash")
	}
}

func TestAdminRevokeAccessRejectsRemovingLastAdmin(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.PostForm(srv.URL+"/admin/access/delete", url.Values{"username": {testAdminUsername}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect, got %d", resp.StatusCode)
	}
	if _, ok := s.cfg.Load().UserByUsername(testAdminUsername); !ok {
		t.Fatal("removing the last admin must be rejected, account should still exist")
	}
}
