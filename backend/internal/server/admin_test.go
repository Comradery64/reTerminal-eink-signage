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

// TestAdminEditRoomPreservesTokenAndWakeOverrideWhenFieldsLeftBlank guards against the exact bug
// this test was added for: editing an existing room by resubmitting the same device_id (the only
// way this form supports edits — it has no per-row prefill) used to silently wipe the room's
// token and wake override whenever those fields were left blank/unchanged, because the handler
// only ever preserved them by looking up original_device_id, which this form never actually
// populates. Left unfixed, this is why only one room in a real fleet kept its business-hours
// override — every other room lost its override the first time anyone touched it here.
func TestAdminEditRoomPreservesTokenAndWakeOverrideWhenFieldsLeftBlank(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	mode := "flat"
	secs := uint32(1800)
	withOverride, err := s.cfg.Load().WithRoomWakeOverride("rt-1", &mode, &secs)
	if err != nil {
		t.Fatal(err)
	}
	s.applyConfig(withOverride, true)
	originalTokenHash := withOverride.Rooms[0].TokenSHA256

	client := loggedInAdminClient(t, srv)
	resp, err := client.PostForm(srv.URL+"/admin/rooms/save", url.Values{
		"device_id": {"rt-1"},
		"name":      {"Aspen (renamed)"}, // the one field actually being changed
		"room":      {"a@x"},
		// token, wake_mode_override, wake_flat_interval_seconds all deliberately left blank
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit room: want 303, got %d", resp.StatusCode)
	}

	room, ok := s.cfg.Load().RoomByDeviceID("rt-1")
	if !ok {
		t.Fatal("room rt-1 missing after edit")
	}
	if room.Name != "Aspen (renamed)" {
		t.Fatalf("name should have been updated, got %q", room.Name)
	}
	if room.TokenSHA256 != originalTokenHash {
		t.Fatalf("token must survive an edit that left the token field blank: got %q, want %q", room.TokenSHA256, originalTokenHash)
	}
	if room.WakeMode == nil || *room.WakeMode != "flat" {
		t.Fatalf("wake mode override must survive an edit that left it unchanged: got %+v", room.WakeMode)
	}
	if room.FlatIntervalSeconds == nil || *room.FlatIntervalSeconds != 1800 {
		t.Fatalf("flat interval override must survive an edit that left it unchanged: got %+v", room.FlatIntervalSeconds)
	}
}

// TestAdminEditRoomCanExplicitlySetOrClearWakeOverride covers the other half: the new fields
// actually work when an admin does want to change or remove the override from /admin, not just
// /manager's per-room Smart/Simple toggle.
func TestAdminEditRoomCanExplicitlySetOrClearWakeOverride(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.PostForm(srv.URL+"/admin/rooms/save", url.Values{
		"device_id":                  {"rt-1"},
		"name":                       {"Aspen"},
		"room":                       {"a@x"},
		"wake_mode_override":         {"flat"},
		"wake_flat_interval_seconds": {"900"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set override: want 303, got %d", resp.StatusCode)
	}
	room, _ := s.cfg.Load().RoomByDeviceID("rt-1")
	if room.WakeMode == nil || *room.WakeMode != "flat" || room.FlatIntervalSeconds == nil || *room.FlatIntervalSeconds != 900 {
		t.Fatalf("override should now be set: %+v / %+v", room.WakeMode, room.FlatIntervalSeconds)
	}

	resp, err = client.PostForm(srv.URL+"/admin/rooms/save", url.Values{
		"device_id":          {"rt-1"},
		"name":               {"Aspen"},
		"room":               {"a@x"},
		"wake_mode_override": {"default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear override: want 303, got %d", resp.StatusCode)
	}
	room, _ = s.cfg.Load().RoomByDeviceID("rt-1")
	if room.WakeMode != nil {
		t.Fatalf("override should be cleared back to fleet default, got %+v", room.WakeMode)
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
	if !strings.Contains(html, testManagerUsername) || !strings.Contains(html, testViewerUsername) {
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
	// An admin-set password forces a change before anything else is reachable — see
	// TestAdminGrantedAccountMustChangePasswordBeforeUsingIt for the full forced-change flow.
	if got := loginResp.Header.Get("Location"); got != "/manager/change-password" {
		t.Fatalf("new manager login: want redirect to change-password, got %q", got)
	}
	page, err := client.Get(srv.URL + "/manager")
	if err != nil || page.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /manager before changing the admin-set password must redirect, got err=%v code=%v", err, page)
	}
}

func TestAdminGrantedAccountMustChangePasswordBeforeUsingIt(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	adminClient := loggedInAdminClient(t, srv)

	if _, err := adminClient.PostForm(srv.URL+"/admin/access/save", url.Values{
		"username": {"newmanager"}, "password": {"temp-pw"}, "role": {"manager"},
	}); err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	client := srv.Client()
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := client.PostForm(srv.URL+"/manager/login", url.Values{"username": {"newmanager"}, "password": {"temp-pw"}}); err != nil {
		t.Fatal(err)
	}

	// Every other page redirects to change-password while the flag is set.
	if page, err := client.Get(srv.URL + "/manager"); err != nil || page.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /manager pre-change: err=%v code=%v", err, page)
	}

	// The change-password page itself must stay reachable (it's the one exception in requireRole).
	if page, err := client.Get(srv.URL + "/manager/change-password"); err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /manager/change-password: err=%v code=%v", err, page)
	}

	resp, err := client.PostForm(srv.URL+"/manager/change-password", url.Values{
		"password": {"my-own-pw"}, "password_confirm": {"my-own-pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Location"); got != "/dashboard" {
		t.Fatalf("change-password: want redirect to /dashboard, got %q", got)
	}

	// The flag is cleared in config too, and the account can now reach /manager directly.
	updated, ok := s.cfg.Load().UserByUsername("newmanager")
	if !ok || updated.MustChangePassword {
		t.Fatalf("MustChangePassword should be cleared after a successful change: %+v", updated)
	}
	if page, err := client.Get(srv.URL + "/manager"); err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /manager post-change: err=%v code=%v", err, page)
	}

	// The old temporary password no longer works.
	jar2, _ := cookiejar.New(nil)
	client2 := srv.Client()
	client2.Jar = jar2
	client2.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	loginResp, err := client2.PostForm(srv.URL+"/manager/login", url.Values{"username": {"newmanager"}, "password": {"temp-pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := loginResp.Header.Get("Location"); got == "/manager" {
		t.Fatal("the old admin-set password must stop working once the account holder changes it")
	}
}

func TestAdminEditAccessChangesRoleWithoutChangingPassword(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp, err := client.PostForm(srv.URL+"/admin/access/save", url.Values{
		"original_username": {testManagerUsername}, "username": {testManagerUsername}, "role": {"viewer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit access: want 303, got %d", resp.StatusCode)
	}

	updated, ok := s.cfg.Load().UserByUsername(testManagerUsername)
	if !ok || updated.Role != "viewer" {
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
