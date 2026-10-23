package server

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

const enrolledSecret = "JBSWY3DPEHPK3PXP"

// require2FA switches the requirement on (the shared test server turns it off) or back off.
func require2FA(s *Server, on bool) {
	c := *s.cfg.Load()
	c.Auth.Require2FA = boolPtr(on)
	s.cfg.Store(&c)
}

func setTOTP(s *Server, user, secret string) {
	c := *s.cfg.Load()
	c.Users = append([]config.User(nil), c.Users...)
	for i := range c.Users {
		if c.Users[i].Username == user {
			c.Users[i].TOTPSecret = secret
		}
	}
	s.cfg.Store(&c)
}

func locationOf(rec interface{ Header() http.Header }) string { return rec.Header().Get("Location") }

// With 2FA required, an account that hasn't enrolled can reach nothing but its own setup page —
// whichever door it comes through — and nothing it POSTs takes effect.
func TestRequired2FASendsUnenrolledAccountsToSetup(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	for _, c := range []struct {
		ui   roleUI
		role auth.Role
		user string
		path string
		want string
	}{
		{managerUI, auth.RoleManager, testManagerUsername, "/manager", "/manager/2fa/setup"},
		{viewerUI, auth.RoleViewer, testViewerUsername, "/dashboard", "/dashboard/2fa/setup"},
		{adminUI, auth.RoleAdmin, testAdminUsername, "/admin", "/admin/2fa/setup"},
	} {
		rec := asAccount(s, c.ui, c.role, c.user, "GET", c.path, nil)
		if rec.Code != http.StatusSeeOther || locationOf(rec) != c.want {
			t.Errorf("%s at %s: got %d -> %q, want redirect to %s", c.user, c.path, rec.Code, locationOf(rec), c.want)
		}
	}
	rec := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "POST", "/manager/wake/save", url.Values{"device_id": {"rt-2"}, "wake_mode": {"smart"}})
	if locationOf(rec) != "/manager/2fa/setup" {
		t.Errorf("a change from an unenrolled account must be redirected, got %q", locationOf(rec))
	}
	if r, _ := s.cfg.Load().RoomByDeviceID("rt-2"); r.WakeMode != nil {
		t.Error("the unenrolled account's change was applied")
	}
}

// Secure by default: no setting means required; only an explicit false turns it off.
func TestRequire2FADefaultsOnAndCanBeTurnedOff(t *testing.T) {
	if !(config.AuthConfig{}).TwoFactorRequired() {
		t.Error("an unset require_2fa must mean required")
	}
	if (config.AuthConfig{Require2FA: boolPtr(false)}).TwoFactorRequired() {
		t.Error("require_2fa: false must turn it off")
	}
	s := scopeServer(t)
	require2FA(s, false)
	if rec := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager", nil); rec.Code != http.StatusOK {
		t.Errorf("with the requirement off, the page must load: %d %s", rec.Code, locationOf(rec))
	}
}

// Accounts that have enrolled are unaffected.
func TestEnrolledAccountPassesTheRequirement(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	setTOTP(s, testManagerUsername, enrolledSecret)
	if rec := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager", nil); rec.Code != http.StatusOK {
		t.Fatalf("enrolled account: %d %s", rec.Code, locationOf(rec))
	}
}

// A forced password change still comes first, so the temporary password is replaced before the
// account is walked through 2FA setup.
func TestForcedPasswordChangeComesBefore2FASetup(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	tok := s.sessions.Create(auth.RoleManager, testManagerUsername, auth.SessionFlags{MustChangePassword: true})
	req := newCookieReq("GET", "/manager", managerUI.cookieName, tok)
	rec := serve(s, req)
	if locationOf(rec) != "/manager/change-password" {
		t.Fatalf("got %q, want the password change first", locationOf(rec))
	}
}

// Enrolling from the manager door: the page explains it's required and posts back to the manager
// door; a wrong code stores nothing; the right one stores the secret and unlocks the account.
func TestManagerEnrolsThroughTheirOwnDoor(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	tok := s.sessions.Create(auth.RoleManager, testManagerUsername, auth.SessionFlags{})
	get := func(path string, form url.Values) *httptest.ResponseRecorder {
		var req = newCookieReq("GET", path, managerUI.cookieName, tok)
		if form != nil {
			req = newCookieFormReq("POST", path, managerUI.cookieName, tok, form)
		}
		return serve(s, req)
	}
	page := get("/manager/2fa/setup", nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, `action="/manager/2fa/enable"`) || !strings.Contains(body, "required for every account") {
		t.Fatalf("setup page: code=%d, missing door-specific form or the required notice", page.Code)
	}
	secret := totpSecretPattern.FindStringSubmatch(body)
	if len(secret) != 2 {
		t.Fatal("no secret on the setup page")
	}
	if rec := get("/manager/2fa/enable", url.Values{"secret": {secret[1]}, "code": {"000000"}}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Incorrect or expired code") {
		t.Fatalf("a wrong code must redisplay the page with an error: %d", rec.Code)
	}
	if u, _ := s.cfg.Load().UserByUsername(testManagerUsername); u.TOTPSecret != "" {
		t.Fatal("a wrong code stored a secret")
	}
	code, _ := auth.TOTPCode(secret[1], time.Now())
	rec := get("/manager/2fa/enable", url.Values{"secret": {secret[1]}, "code": {code}})
	if rec.Code != http.StatusSeeOther || locationOf(rec) != "/manager" {
		t.Fatalf("enable: %d -> %q", rec.Code, locationOf(rec))
	}
	if u, _ := s.cfg.Load().UserByUsername(testManagerUsername); u.TOTPSecret != secret[1] {
		t.Fatal("the confirmed secret was not stored")
	}
	if rec := get("/manager", nil); rec.Code != http.StatusOK {
		t.Fatalf("after enrolling the account must be let in: %d %s", rec.Code, locationOf(rec))
	}
}

// Viewers enrol through their own door too (they had no setup page at all before).
func TestViewerCanEnrol(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	rec := asAccount(s, viewerUI, auth.RoleViewer, testViewerUsername, "GET", "/dashboard/2fa/setup", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/dashboard/2fa/enable"`) {
		t.Fatalf("viewer setup page: %d", rec.Code)
	}
}

// Enrolment needs a session: with none, both the page and the POST go to the login page.
func TestEnrolmentNeedsASession(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	for _, path := range []string{"/manager/2fa/setup", "/manager/2fa/enable"} {
		method := "GET"
		if strings.HasSuffix(path, "enable") {
			method = "POST"
		}
		rec := serve(s, newCookieReq(method, path, "", ""))
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(locationOf(rec), "/manager/login") {
			t.Errorf("%s %s without a session: %d -> %q", method, path, rec.Code, locationOf(rec))
		}
	}
}

// While it's required an account can't switch its own 2FA off, even with a valid code.
func TestRequired2FACannotBeDisabledByTheAccount(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	setTOTP(s, testAdminUsername, enrolledSecret)
	code, _ := auth.TOTPCode(enrolledSecret, time.Now())
	rec := adminPost(t, s.Handler(), s, "/admin/2fa/disable", url.Values{"code": {code}})
	if !strings.Contains(locationOf(rec), "error") {
		t.Fatalf("disable must be refused while required: %q", locationOf(rec))
	}
	if u, _ := s.cfg.Load().UserByUsername(testAdminUsername); u.TOTPSecret != enrolledSecret {
		t.Fatal("2FA was disabled anyway")
	}
	if page := adminGet(t, s.Handler(), s, "/admin"); strings.Contains(page, "Disable 2FA") {
		t.Error("the Security section still offers Disable while 2FA is required")
	}
}

// An admin resets another account's 2FA (lost phone): the secret goes, the account's sessions are
// revoked, and its next login is sent to set up again. Not their own, not an account without 2FA.
func TestAdminReset2FA(t *testing.T) {
	s := scopeServer(t)
	require2FA(s, true)
	setTOTP(s, testManagerUsername, enrolledSecret)
	setTOTP(s, testAdminUsername, enrolledSecret)
	h := s.Handler()
	victim := s.sessions.Create(auth.RoleManager, testManagerUsername, auth.SessionFlags{})

	// Offered for the other enrolled account only: not for the admin's own row, nor for the viewer
	// who has no 2FA to reset.
	if page := adminGet(t, h, s, "/admin"); strings.Count(page, `action="/admin/access/reset-2fa"`) != 1 {
		t.Errorf("Access must offer Reset 2FA exactly once (for the manager), got %d", strings.Count(page, `action="/admin/access/reset-2fa"`))
	}
	if rec := adminPost(t, h, s, "/admin/access/reset-2fa", url.Values{"username": {testAdminUsername}}); !strings.Contains(locationOf(rec), "error") {
		t.Error("resetting your own 2FA must be refused")
	}
	if rec := adminPost(t, h, s, "/admin/access/reset-2fa", url.Values{"username": {testViewerUsername}}); !strings.Contains(locationOf(rec), "error") {
		t.Error("resetting an account with no 2FA must be refused")
	}
	rec := adminPost(t, h, s, "/admin/access/reset-2fa", url.Values{"username": {testManagerUsername}})
	if locationOf(rec) != "/admin?saved=access" {
		t.Fatalf("reset: %q", locationOf(rec))
	}
	if u, _ := s.cfg.Load().UserByUsername(testManagerUsername); u.TOTPSecret != "" {
		t.Error("the secret was not cleared")
	}
	if _, ok := s.sessions.Check(victim); ok {
		t.Error("the account's live session survived the reset")
	}
	if u, _ := s.cfg.Load().UserByUsername(testAdminUsername); u.TOTPSecret != enrolledSecret {
		t.Error("the admin's own 2FA changed")
	}
	rec = asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager", nil)
	if locationOf(rec) != "/manager/2fa/setup" {
		t.Errorf("after a reset the account must be sent to set up again, got %q", locationOf(rec))
	}
	// Only admins can do this.
	if rec := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "POST", "/admin/access/reset-2fa", url.Values{"username": {testAdminUsername}}); rec.Code != http.StatusSeeOther || strings.Contains(locationOf(rec), "saved") {
		t.Errorf("a manager must not reach reset-2fa: %d %q", rec.Code, locationOf(rec))
	}
}

func newCookieReq(method, path, cookie, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookie, Value: token})
	}
	return req
}

func newCookieFormReq(method, path, cookie, token string, form url.Values) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookie, Value: token})
	return req
}

func serve(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// A forced password change must not loop with the 2FA pages: the change-password page is served,
// and submitting it moves on to 2FA setup (or verification for an enrolled account). Walked with a
// real cookie jar through every door, because a one-request check cannot see a redirect loop.
func TestForcedPasswordChangeThen2FADoesNotLoop(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		for _, c := range []struct {
			ui   roleUI
			next string
		}{{managerUI, "/manager"}, {viewerUI, "/dashboard"}} {
			s := scopeServer(t)
			require2FA(s, true)
			cfg := *s.cfg.Load()
			cfg.Users = append([]config.User(nil), cfg.Users...)
			for i := range cfg.Users {
				if cfg.Users[i].Username == testManagerUsername {
					cfg.Users[i].MustChangePassword = true
					if enrolled {
						cfg.Users[i].TOTPSecret = enrolledSecret
					}
				}
			}
			s.cfg.Store(&cfg)
			s.refreshDerived(&cfg)
			srv := httptest.NewTLSServer(s.Handler())
			jar, _ := cookiejar.New(nil)
			cl := srv.Client()
			cl.Jar = jar
			cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

			resp, err := cl.PostForm(srv.URL+c.ui.loginPath, url.Values{"username": {testManagerUsername}, "password": {testManagerPassword}})
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Header.Get("Location"); got != c.ui.changePasswordPath {
				t.Fatalf("enrolled=%v %s: login sent to %q, want %s", enrolled, c.ui.label, got, c.ui.changePasswordPath)
			}
			resp, err = cl.Get(srv.URL + c.ui.changePasswordPath)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("enrolled=%v %s: the change-password page must be served, got %d -> %q", enrolled, c.ui.label, resp.StatusCode, resp.Header.Get("Location"))
			}
			resp, err = cl.PostForm(srv.URL+c.ui.changePasswordPath, url.Values{"password": {"new-pass-1"}, "password_confirm": {"new-pass-1"}})
			if err != nil {
				t.Fatal(err)
			}
			want := viewerUI.homePath
			if enrolled {
				want = c.ui.totpVerifyPath
			}
			if got := resp.Header.Get("Location"); got != want {
				t.Fatalf("enrolled=%v %s: after changing the password got %q, want %s", enrolled, c.ui.label, got, want)
			}
			// Follow what is left; it must settle on a page within a few hops.
			for hop := 0; resp.StatusCode == http.StatusSeeOther; hop++ {
				if hop > 4 {
					t.Fatalf("enrolled=%v %s: redirect loop, still at %q", enrolled, c.ui.label, resp.Header.Get("Location"))
				}
				resp, err = cl.Get(srv.URL + resp.Header.Get("Location"))
				if err != nil {
					t.Fatal(err)
				}
			}
			srv.Close()
		}
	}
}
