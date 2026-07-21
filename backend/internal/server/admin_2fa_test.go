package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
)

var totpSecretPattern = regexp.MustCompile(`<div class="secret">([A-Z0-9]+)</div>`)

// enrollTOTP drives the full setup page -> enable flow and returns the secret now stored on the
// admin account, so a test can compute valid codes for subsequent logins.
func enrollTOTP(t *testing.T, client *http.Client, srv *httptest.Server) string {
	t.Helper()
	setupResp, err := client.Get(srv.URL + "/admin/2fa/setup")
	if err != nil || setupResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/2fa/setup: err=%v code=%v", err, setupResp)
	}
	body, _ := io.ReadAll(setupResp.Body)
	m := totpSecretPattern.FindStringSubmatch(string(body))
	if len(m) != 2 {
		t.Fatalf("could not find secret in setup page: %s", body)
	}
	secret := m[1]

	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	enableResp, err := client.PostForm(srv.URL+"/admin/2fa/enable", url.Values{"secret": {secret}, "code": {code}})
	if err != nil {
		t.Fatal(err)
	}
	if got := enableResp.Header.Get("Location"); got != "/admin?saved=security#security" {
		t.Fatalf("enable 2fa: want redirect to saved=security, got %q (status %d)", got, enableResp.StatusCode)
	}
	return secret
}

func TestTOTPSetupThenEnableStoresSecretAndRequiresCodeOnNextLogin(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	secret := enrollTOTP(t, client, srv)

	updated, ok := s.cfg.Load().UserByUsername(testAdminUsername)
	if !ok || updated.TOTPSecret != secret {
		t.Fatalf("TOTPSecret not persisted: %+v", updated)
	}

	// A fresh login (password only) must land on verify-2fa, not straight on /admin.
	jar, _ := cookiejar.New(nil)
	fresh := srv.Client()
	fresh.Jar = jar
	fresh.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	loginResp, err := fresh.PostForm(srv.URL+"/admin/login", url.Values{"username": {testAdminUsername}, "password": {testAdminPassword}})
	if err != nil {
		t.Fatal(err)
	}
	if got := loginResp.Header.Get("Location"); got != "/admin/verify-2fa" {
		t.Fatalf("login with 2fa enabled: want redirect to verify-2fa, got %q", got)
	}
	if page, err := fresh.Get(srv.URL + "/admin"); err != nil || page.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /admin before verifying 2fa must redirect, got err=%v code=%v", err, page)
	}

	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verifyResp, err := fresh.PostForm(srv.URL+"/admin/verify-2fa", url.Values{"code": {code}})
	if err != nil {
		t.Fatal(err)
	}
	if got := verifyResp.Header.Get("Location"); got != "/admin" {
		t.Fatalf("verify-2fa with correct code: want redirect to /admin, got %q", got)
	}
	if page, err := fresh.Get(srv.URL + "/admin"); err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin after verifying 2fa: err=%v code=%v", err, page)
	}
}

func TestTOTPVerifyRejectsWrongCode(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)
	enrollTOTP(t, client, srv)

	jar, _ := cookiejar.New(nil)
	fresh := srv.Client()
	fresh.Jar = jar
	fresh.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := fresh.PostForm(srv.URL+"/admin/login", url.Values{"username": {testAdminUsername}, "password": {testAdminPassword}}); err != nil {
		t.Fatal(err)
	}

	resp, err := fresh.PostForm(srv.URL+"/admin/verify-2fa", url.Values{"code": {"000000"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Location"); !strings.HasPrefix(got, "/admin/verify-2fa?error=") {
		t.Fatalf("wrong code: want redirect back to verify-2fa with an error, got %q", got)
	}
	if page, err := fresh.Get(srv.URL + "/admin"); err != nil || page.StatusCode != http.StatusSeeOther {
		t.Fatalf("a wrong code must not grant access to /admin: err=%v code=%v", err, page)
	}
}

func TestTOTPDisableRequiresCurrentCode(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)
	secret := enrollTOTP(t, client, srv)

	// Wrong code: 2FA must stay enabled.
	if _, err := client.PostForm(srv.URL+"/admin/2fa/disable", url.Values{"code": {"000000"}}); err != nil {
		t.Fatal(err)
	}
	if u, ok := s.cfg.Load().UserByUsername(testAdminUsername); !ok || u.TOTPSecret == "" {
		t.Fatal("2FA must remain enabled after a disable attempt with the wrong code")
	}

	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.PostForm(srv.URL+"/admin/2fa/disable", url.Values{"code": {code}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Location"); got != "/admin?saved=security#security" {
		t.Fatalf("disable 2fa: want redirect to saved=security, got %q", got)
	}
	if u, ok := s.cfg.Load().UserByUsername(testAdminUsername); !ok || u.TOTPSecret != "" {
		t.Fatal("2FA must be disabled after a correct-code disable request")
	}
}
