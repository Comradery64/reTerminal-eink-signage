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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

const testForgotPasswordUsername = "recoverable-admin"
const testForgotPasswordEmail = "recoverable-admin@example.com"

// fakeEmailSender records every call instead of opening a network connection — this is how every
// test in this file asserts on the forgot-password flow without ever sending real email, matching
// the instruction to build the feature without exercising a real relay.
type fakeEmailSender struct {
	mu   sync.Mutex
	sent []fakeEmail
}

type fakeEmail struct {
	host, from, to, subject, body string
	port                          int
}

func (f *fakeEmailSender) Send(host string, port int, from, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, fakeEmail{host: host, port: port, from: from, to: to, subject: subject, body: body})
	return nil
}

func (f *fakeEmailSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// last returns the most recently recorded email, or the zero value if none was sent.
func (f *fakeEmailSender) last() fakeEmail {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return fakeEmail{}
	}
	return f.sent[len(f.sent)-1]
}

// testServerWithPasswordResetEnabled mirrors testServerWithAuth (login_test.go) but turns on
// auth.password_reset and seeds one user with an Email on file — the one config shape the rest of
// this file's tests need that the shared helper deliberately doesn't provide, since password
// reset is off by default everywhere else.
func testServerWithPasswordResetEnabled(t *testing.T) (*Server, *fakeEmailSender) {
	t.Helper()
	sum := sha256.Sum256([]byte(testToken))
	cfg := &config.Config{
		Provider: "google",
		Rooms:    []config.Room{{DeviceID: "rt-1", Name: "Aspen", Room: "a@x", TokenSHA256: hex.EncodeToString(sum[:])}},
		Users: []config.User{
			{Username: testForgotPasswordUsername, PasswordSHA256: hashHex("original-password"), Role: "admin", Email: testForgotPasswordEmail},
			{Username: "no-email-user", PasswordSHA256: hashHex("whatever"), Role: "viewer"},
		},
		Auth: config.AuthConfig{
			SessionSecret: "01234567890123456789012345678901",
			PasswordReset: config.PasswordResetConfig{
				Enabled: true, SMTPHost: "smtp-relay.gmail.com", SMTPPort: 587,
				FromAddress: "noreply@example.com", PublicBaseURL: "https://displays.example.com",
			},
		},
	}
	cfg.Wake.Timezone = "UTC"
	cfg.Alerts = config.AlertConfig{LowBatteryPct: 45, ClearPct: 55, MinRenotify: 24 * time.Hour, StaleAfter: time.Hour}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	alerts := notify.NewManager("", 45, 55, 24*time.Hour, log)
	s := New(config.NewLive(cfg), cache.New(), telemetry.New(), alerts, noopPersistStore{}, log)
	sender := &fakeEmailSender{}
	s.SetEmailSender(sender)
	return s, sender
}

func freshClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport:     srv.Client().Transport,
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestForgotPasswordIs404WhenDisabled(t *testing.T) {
	s := testServerWithAuth(t) // password_reset.enabled defaults to false
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := freshClient(t, srv)

	for _, path := range []string{"/forgot-password", "/reset-password"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with password_reset disabled: want 404, got %d", path, resp.StatusCode)
		}
	}
}

func TestForgotPasswordSendsEmailOnlyForAccountWithEmailOnFile(t *testing.T) {
	s, sender := testServerWithPasswordResetEnabled(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := freshClient(t, srv)

	// Unknown username, known username with no email, and the real account: all three must
	// produce the identical redirect — no enumeration signal either way.
	var locations []string
	for _, username := range []string{"no-such-user", "no-email-user", testForgotPasswordUsername} {
		resp, err := client.PostForm(srv.URL+"/forgot-password", url.Values{"username": {username}})
		if err != nil {
			t.Fatal(err)
		}
		locations = append(locations, resp.Header.Get("Location"))
	}
	for i, loc := range locations {
		if loc != "/forgot-password?sent=1" {
			t.Errorf("response %d: want identical generic redirect, got %q", i, loc)
		}
	}

	if sender.count() != 1 {
		t.Fatalf("want exactly 1 email sent (only the account with an Email on file), got %d", sender.count())
	}
	sent := sender.last()
	if sent.to != testForgotPasswordEmail {
		t.Errorf("email sent to %q, want %q", sent.to, testForgotPasswordEmail)
	}
	if sent.host != "smtp-relay.gmail.com" || sent.port != 587 {
		t.Errorf("email relay = %s:%d, want smtp-relay.gmail.com:587", sent.host, sent.port)
	}
	if !strings.Contains(sent.body, "https://displays.example.com/reset-password?token=") {
		t.Errorf("email body missing a reset link: %q", sent.body)
	}
}

func TestResetWithTokenSetsPasswordRevokesSessionsAndIsSingleUse(t *testing.T) {
	s, sender := testServerWithPasswordResetEnabled(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := freshClient(t, srv)

	// A session that must not survive the reset.
	oldSession := s.sessions.Create(auth.RoleAdmin, testForgotPasswordUsername, auth.SessionFlags{})
	if _, ok := s.sessions.Check(oldSession); !ok {
		t.Fatal("setup: session should be alive before the reset")
	}

	if _, err := client.PostForm(srv.URL+"/forgot-password", url.Values{"username": {testForgotPasswordUsername}}); err != nil {
		t.Fatal(err)
	}
	body := sender.last().body
	token := extractToken(t, body)

	newPassword := "a-brand-new-password"
	resp, err := client.PostForm(srv.URL+"/reset-password", url.Values{"token": {token}, "password": {newPassword}})
	if err != nil {
		t.Fatal(err)
	}
	if loc := resp.Header.Get("Location"); loc != "/dashboard/login?reset=1" {
		t.Fatalf("want redirect to /dashboard/login?reset=1, got %q", loc)
	}

	u, ok := s.cfg.Load().UserByUsername(testForgotPasswordUsername)
	if !ok {
		t.Fatal("user vanished")
	}
	if u.PasswordSHA256 == hashHex("original-password") {
		t.Fatal("password hash did not change")
	}
	wantHash := hashHex(newPassword)
	if u.PasswordSHA256 != wantHash {
		t.Fatalf("password hash = %q, want %q", u.PasswordSHA256, wantHash)
	}
	if u.MustChangePassword {
		t.Fatal("a self-chosen password via emailed reset must not force yet another change")
	}
	if _, ok := s.sessions.Check(oldSession); ok {
		t.Fatal("the pre-reset session must not survive an emailed password reset")
	}

	// Reusing the same token must fail.
	resp2, err := client.PostForm(srv.URL+"/reset-password", url.Values{"token": {token}, "password": {"another-password-123"}})
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body2), "invalid or has expired") {
		t.Fatal("reusing a consumed reset token must be rejected")
	}
}

func TestForgotPasswordRateLimitsRepeatedRequests(t *testing.T) {
	s, sender := testServerWithPasswordResetEnabled(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := freshClient(t, srv)

	for i := 0; i < forgotPasswordRateLimit+5; i++ {
		if _, err := client.PostForm(srv.URL+"/forgot-password", url.Values{"username": {testForgotPasswordUsername}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := sender.count(); got > forgotPasswordRateLimit {
		t.Fatalf("sent %d emails for repeated requests, want at most %d (rate limit)", got, forgotPasswordRateLimit)
	}
}

func extractToken(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "token=")
	if i < 0 {
		t.Fatalf("no token in email body: %q", body)
	}
	rest := body[i+len("token="):]
	end := strings.IndexAny(rest, "\r\n ")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}
