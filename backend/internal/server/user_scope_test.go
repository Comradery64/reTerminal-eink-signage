package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// Editing an account in /admin must not wipe the 2FA secret or recovery email the account
// holder set themselves; only the fields the form carries may change.
func TestAdminSaveUserKeepsTOTPAndEmail(t *testing.T) {
	s := testServerWithAuth(t)
	c := *s.cfg.Load()
	c.Users = append([]config.User(nil), c.Users...)
	for i := range c.Users {
		if c.Users[i].Username == testManagerUsername {
			c.Users[i].TOTPSecret = "JBSWY3DPEHPK3PXP"
			c.Users[i].Email = "mgr@example.com"
		}
	}
	s.cfg.Store(&c)
	rec := adminPost(t, s.Handler(), s, "/admin/access/save", url.Values{"username": {testManagerUsername}, "original_username": {testManagerUsername}, "role": {"viewer"}})
	if strings.Contains(rec.Header().Get("Location"), "error") {
		t.Fatalf("save failed: %s", rec.Header().Get("Location"))
	}
	u, _ := s.cfg.Load().UserByUsername(testManagerUsername)
	if u.Role != "viewer" || u.TOTPSecret != "JBSWY3DPEHPK3PXP" || u.Email != "mgr@example.com" {
		t.Fatalf("edit wiped self-service fields: role=%q totp_set=%v email_set=%v", u.Role, u.TOTPSecret != "", u.Email != "")
	}
}

// scopeServer has two rooms: Aspen (rt-1) in the default location HQ, Birch (rt-2) in SF. The
// manager and the viewer are each limited to SF.
func scopeServer(t *testing.T) *Server {
	t.Helper()
	s := testServerWithAuth(t)
	withLocations(t, s, config.DefaultLarkBaseURL)
	c := *s.cfg.Load()
	c.Rooms = append(append([]config.Room(nil), c.Rooms...), config.Room{DeviceID: "rt-2", Name: "Birch", Room: "b@x", TokenSHA256: hashHex("tok-2"), Location: "loc-sf"})
	c.Users = append([]config.User(nil), c.Users...)
	for i := range c.Users {
		if c.Users[i].Role != "admin" {
			c.Users[i].Locations = []string{"loc-sf"}
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s.cfg.Store(&c)
	return s
}

func asAccount(s *Server, ui roleUI, role auth.Role, user, method, path string, form url.Values) *httptest.ResponseRecorder {
	tok := s.sessions.Create(role, user, auth.SessionFlags{})
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.AddCookie(&http.Cookie{Name: ui.cookieName, Value: tok})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func setUserLocations(s *Server, user string, locs []string) {
	c := *s.cfg.Load()
	c.Users = append([]config.User(nil), c.Users...)
	for i := range c.Users {
		if c.Users[i].Username == user {
			c.Users[i].Locations = locs
		}
	}
	s.cfg.Store(&c)
}

// A limited manager sees only their location's rooms and can't change another location's room.
func TestManagerLimitedToTheirLocations(t *testing.T) {
	s := scopeServer(t)
	body := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager", nil).Body.String()
	if !strings.Contains(body, "Birch") || strings.Contains(body, "Aspen") {
		t.Fatalf("manager limited to SF must see Birch and not Aspen (page has Birch=%v Aspen=%v)", strings.Contains(body, "Birch"), strings.Contains(body, "Aspen"))
	}
	rec := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "POST", "/manager/wake/save", url.Values{"device_id": {"rt-1"}, "wake_mode": {"smart"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("changing another location's room: got %d, want 403", rec.Code)
	}
	if r, _ := s.cfg.Load().RoomByDeviceID("rt-1"); r.WakeMode != nil {
		t.Error("the refused change was applied anyway")
	}
	rec = asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "POST", "/manager/wake/save", url.Values{"device_id": {"rt-2"}, "wake_mode": {"smart"}})
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "error") {
		t.Fatalf("changing their own location's room: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

// With no list an account sees every location, exactly as before this feature.
func TestUnlimitedAccountSeesEveryLocation(t *testing.T) {
	s := scopeServer(t)
	setUserLocations(s, testManagerUsername, nil)
	body := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager", nil).Body.String()
	if !strings.Contains(body, "Birch") || !strings.Contains(body, "Aspen") {
		t.Fatal("an account with no location list must see every room")
	}
}

// Changing someone's locations applies to a session that is already open.
func TestLocationChangeAppliesToOpenSessions(t *testing.T) {
	s := scopeServer(t)
	tok := s.sessions.Create(auth.RoleManager, testManagerUsername, auth.SessionFlags{})
	get := func() string {
		req := httptest.NewRequest("GET", "/manager", nil)
		req.AddCookie(&http.Cookie{Name: managerUI.cookieName, Value: tok})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if strings.Contains(get(), "Aspen") {
		t.Fatal("limited manager sees Aspen")
	}
	setUserLocations(s, testManagerUsername, []string{"loc-hq", "loc-sf"})
	if !strings.Contains(get(), "Aspen") {
		t.Error("widening the list did not reach the open session")
	}
}

// A session whose account was removed sees nothing rather than everything.
func TestRemovedAccountSessionSeesNothing(t *testing.T) {
	s := scopeServer(t)
	tok := s.sessions.Create(auth.RoleManager, "ghost", auth.SessionFlags{})
	req := httptest.NewRequest("GET", "/manager", nil)
	req.AddCookie(&http.Cookie{Name: managerUI.cookieName, Value: tok})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if b := rec.Body.String(); strings.Contains(b, "Aspen") || strings.Contains(b, "Birch") {
		t.Fatal("an account that no longer exists must not see rooms")
	}
}

// The dashboard lists only the viewer's rooms, and a preview of another location's room is a
// 404 — the same answer as a room that doesn't exist.
func TestViewerDashboardAndPreviewLimited(t *testing.T) {
	s := scopeServer(t)
	for _, id := range []string{"rt-1", "rt-2"} {
		s.cache.Set(id, cache.Entry{PreviewPNG: []byte("png-" + id)})
	}
	body := asAccount(s, viewerUI, auth.RoleViewer, testViewerUsername, "GET", "/dashboard", nil).Body.String()
	if !strings.Contains(body, "Birch") || strings.Contains(body, "Aspen") {
		t.Fatal("viewer limited to SF must see only Birch")
	}
	if rec := asAccount(s, viewerUI, auth.RoleViewer, testViewerUsername, "GET", "/dashboard/preview/rt-1", nil); rec.Code != http.StatusNotFound {
		t.Errorf("preview of another location's room: got %d, want 404", rec.Code)
	}
	if rec := asAccount(s, viewerUI, auth.RoleViewer, testViewerUsername, "GET", "/dashboard/preview/rt-2", nil); rec.Code != http.StatusOK || rec.Body.String() != "png-rt-2" {
		t.Errorf("preview of their own room: got %d", rec.Code)
	}
}

// Admins always see everything, whatever the config says about the account.
func TestAdminSeesEveryLocation(t *testing.T) {
	s := scopeServer(t)
	body := asAccount(s, viewerUI, auth.RoleAdmin, testAdminUsername, "GET", "/dashboard", nil).Body.String()
	if !strings.Contains(body, "Aspen") || !strings.Contains(body, "Birch") {
		t.Fatal("admin must see every room")
	}
}

// Config rules: an admin can't be limited, a list can't name an unknown or repeated location,
// and a location someone is limited to can't be deleted (an empty list would mean "everything").
func TestUserLocationValidation(t *testing.T) {
	s := scopeServer(t)
	base := s.cfg.Load()
	for name, mutate := range map[string]func(u *config.User){
		"admin with a list": func(u *config.User) { u.Role = "admin"; u.Locations = []string{"loc-sf"} },
		"unknown location":  func(u *config.User) { u.Locations = []string{"nowhere"} },
		"repeated location": func(u *config.User) { u.Locations = []string{"loc-sf", "loc-sf"} },
	} {
		u, _ := base.UserByUsername(testManagerUsername)
		mutate(&u)
		if _, err := base.WithUser(u); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// loc-sf has a room too, so move it first; the user list is then the only thing keeping it.
	c := *base
	c.Rooms = append([]config.Room(nil), c.Rooms...)
	c.Rooms[1].Location = "loc-hq"
	if _, err := c.WithoutLocation("loc-sf"); err == nil || !strings.Contains(err.Error(), "limited to location") {
		t.Fatalf("deleting a location a user is limited to must be refused, got %v", err)
	}
	setUserLocations(s, testManagerUsername, nil)
	setUserLocations(s, testViewerUsername, nil)
	c2 := *s.cfg.Load()
	c2.Rooms = c.Rooms
	if _, err := c2.WithoutLocation("loc-sf"); err != nil {
		t.Errorf("with nobody limited to it, delete should work: %v", err)
	}
}

// /admin Access saves the ticked locations, lists them, and an unticked form means everything.
func TestAdminAccessSavesLocations(t *testing.T) {
	s := scopeServer(t)
	h := s.Handler()
	rec := adminPost(t, h, s, "/admin/access/save", url.Values{"username": {testManagerUsername}, "original_username": {testManagerUsername}, "role": {"manager"}, "locations": {"loc-hq", "loc-sf"}})
	if strings.Contains(rec.Header().Get("Location"), "error") {
		t.Fatalf("save failed: %s", rec.Header().Get("Location"))
	}
	if u, _ := s.cfg.Load().UserByUsername(testManagerUsername); len(u.Locations) != 2 {
		t.Fatalf("locations = %v", u.Locations)
	}
	if page := adminGet(t, h, s, "/admin"); !strings.Contains(page, `name="locations" value="loc-hq"`) || !strings.Contains(page, `data-locations="loc-hq,loc-sf,"`) {
		t.Error("/admin Access must render the location checkboxes and the Edit prefill")
	}
	adminPost(t, h, s, "/admin/access/save", url.Values{"username": {testManagerUsername}, "original_username": {testManagerUsername}, "role": {"manager"}})
	if u, _ := s.cfg.Load().UserByUsername(testManagerUsername); len(u.Locations) != 0 {
		t.Errorf("nothing ticked must mean every location, got %v", u.Locations)
	}
	rec = adminPost(t, h, s, "/admin/access/save", url.Values{"username": {testManagerUsername}, "original_username": {testManagerUsername}, "role": {"admin"}, "locations": {"loc-sf"}})
	if !strings.Contains(rec.Header().Get("Location"), "error") {
		t.Error("an admin limited to a location must be refused")
	}
}

// A limited account can only subscribe to push for their own locations (never "every location"),
// and a subscription kept from before the limit stops delivering other offices' alerts.
func TestPushLimitedToTheirLocations(t *testing.T) {
	s := scopeServer(t)
	s.vapidPublicKey = vapidConfig(t).VAPIDPublicKey
	h := s.Handler()
	tok := s.sessions.Create(managerUI.role, testManagerUsername, auth.SessionFlags{})
	sub := func(endpoint, loc string) int {
		p, a := browserKeys(t)
		return postJSON(t, h, "/manager/api/push/subscribe", tok, map[string]any{"endpoint": endpoint, "location": loc, "keys": map[string]string{"p256dh": p, "auth": a}}).Code
	}
	if code := sub("https://push.example/hq", "loc-hq"); code != http.StatusForbidden {
		t.Errorf("another location: got %d, want 403", code)
	}
	if code := sub("https://push.example/all", ""); code != http.StatusForbidden {
		t.Errorf("every location: got %d, want 403", code)
	}
	if code := sub("https://push.example/sf", "loc-sf"); code != http.StatusNoContent {
		t.Errorf("own location: got %d, want 204", code)
	}

	// Send time: a subscription for every location, made while the manager was unlimited, is
	// dropped for HQ alerts once they're limited to SF, and still delivers SF alerts.
	setUserLocations(s, testManagerUsername, nil)
	c := s.cfg.Load()
	p, a := browserKeys(t)
	c, err := c.WithPushSubscription(config.PushSubscription{Username: testManagerUsername, Location: "", Endpoint: "https://push.example/old", P256DH: p, Auth: a})
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store(c)
	setUserLocations(s, testManagerUsername, []string{"loc-sf"})
	has := func(loc, endpoint string) bool {
		for _, sub := range s.PushSubscriptions(loc) {
			if sub.Endpoint == endpoint {
				return true
			}
		}
		return false
	}
	if has("loc-hq", "https://push.example/old") {
		t.Error("a limited account still gets another office's alerts through an old subscription")
	}
	if !has("loc-sf", "https://push.example/old") {
		t.Error("a limited account must still get their own office's alerts")
	}
}

// The notifications page doesn't offer a limited account other offices or "Every location".
func TestNotificationsPageOffersOnlyAllowedLocations(t *testing.T) {
	s := scopeServer(t)
	s.vapidPublicKey = vapidConfig(t).VAPIDPublicKey
	body := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager/notifications", nil).Body.String()
	if !strings.Contains(body, `value="loc-sf"`) || strings.Contains(body, `value="loc-hq"`) || strings.Contains(body, "Every location") {
		t.Errorf("limited account's options wrong: sf=%v hq=%v every=%v", strings.Contains(body, `value="loc-sf"`), strings.Contains(body, `value="loc-hq"`), strings.Contains(body, "Every location"))
	}
	setUserLocations(s, testManagerUsername, nil)
	body = asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager/notifications", nil).Body.String()
	if !strings.Contains(body, `value="loc-hq"`) || !strings.Contains(body, "Every location") {
		t.Error("an unlimited account must keep every option")
	}
}
