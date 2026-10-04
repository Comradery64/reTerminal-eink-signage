package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// resetTokenTTL bounds how long a mailed reset link stays valid. Long enough to actually receive
// and open an email, short enough that an abandoned request doesn't leave a live credential
// sitting in an inbox indefinitely.
const resetTokenTTL = 30 * time.Minute

// forgotPasswordRateLimit and forgotPasswordRateWindow bound how often one username or one source
// IP can trigger an email send — the relay is shared across the whole Workspace domain, not just
// this broker, so an unbounded loop here is everyone's problem, not just this account's.
const (
	forgotPasswordRateLimit  = 3
	forgotPasswordRateWindow = 15 * time.Minute
)

// resetToken is one single-use password-reset link. Never logged, never written to disk — same
// posture as provisionArtifact's device token and passwordReset's temporary password.
type resetToken struct {
	username string
	expires  time.Time
}

// resetTokenStore mirrors provisionStore's nonce/TTL/sweep-on-write shape.
type resetTokenStore struct {
	mu sync.Mutex
	m  map[string]*resetToken
}

func newResetTokenStore() *resetTokenStore { return &resetTokenStore{m: map[string]*resetToken{}} }

func (s *resetTokenStore) put(token string, r *resetToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.expires) {
			delete(s.m, k)
		}
	}
	s.m[token] = r
}

// peek checks validity without consuming — used to decide whether to show the "set a new
// password" form at all.
func (s *resetTokenStore) peek(token string) (*resetToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[token]
	if !ok || time.Now().After(r.expires) {
		return nil, false
	}
	return r, true
}

// take consumes the token — a password-reset link is good for exactly one submission.
func (s *resetTokenStore) take(token string) (*resetToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[token]
	if !ok {
		return nil, false
	}
	delete(s.m, token)
	if time.Now().After(r.expires) {
		return nil, false
	}
	return r, true
}

// rateLimiter is a tiny fixed-window counter keyed by an arbitrary string (username or source IP).
// Sized for "a handful of IT/building-staff accounts" (see auth.SessionStore's own doc comment) —
// not built to survive a restart or scale past this fleet's account count.
type rateLimiter struct {
	mu     sync.Mutex
	counts map[string]*rateWindow
}

type rateWindow struct {
	count int
	reset time.Time
}

func newRateLimiter() *rateLimiter { return &rateLimiter{counts: map[string]*rateWindow{}} }

// allow reports whether key is still under the limit, incrementing its count either way so a
// blocked caller doesn't get a free extra attempt by retrying.
func (r *rateLimiter) allow(key string, limit int, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	w, ok := r.counts[key]
	if !ok || now.After(w.reset) {
		w = &rateWindow{count: 0, reset: now.Add(window)}
		r.counts[key] = w
	}
	w.count++
	return w.count <= limit
}

// emailSender abstracts the one outbound side effect in this file, so tests can inject a fake
// instead of exercising a real SMTP relay — see Server.SetEmailSender.
type emailSender interface {
	Send(host string, port int, from, to, subject, body string) error
}

// smtpRelaySender talks to a relay that authenticates by source-IP allowlist rather than a stored
// credential (e.g. Google Workspace's SMTP relay service) — there is deliberately no Auth field or
// password anywhere in this type. smtp.SendMail with a nil auth still negotiates STARTTLS if the
// server offers it; it simply skips the AUTH step entirely.
type smtpRelaySender struct{}

func (smtpRelaySender) Send(host string, port int, from, to, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		from, to, subject, body)
	return smtp.SendMail(addr, nil, from, []string{to}, []byte(msg))
}

// SetEmailSender overrides the relay implementation — tests use this to assert an email WOULD
// have been sent, with what content, without ever opening a real network connection. Defaults to
// smtpRelaySender{}, which is never reached unless auth.password_reset.enabled is true.
func (s *Server) SetEmailSender(e emailSender) { s.emailSender = e }

type forgotPasswordPageView struct {
	Sent  bool // true on the generic "check your email" confirmation
	Error string
}

var forgotPasswordPageTmpl = template.Must(template.New("forgot-password").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Forgot password — Meeting display fleet</title>
<style>` + baseCSS + `
body { display: flex; align-items: center; justify-content: center; min-height: 100vh; padding: var(--space-5); }
.login-card { width: 100%; max-width: 22rem; padding: var(--space-6); }
.login-card .brand { margin-bottom: var(--space-5); }
form { margin-top: var(--space-5); }
input[type=text] { display: block; width: 100%; margin-bottom: var(--space-4); }
button[type=submit] { width: 100%; }
</style>
</head>
<body>
<div class="surface login-card">
` + brandMark + `
<h1>Forgot password</h1>
{{if .Sent}}
<p>If that account exists and has an email address on file, a password-reset link is on its way.
It expires in 30 minutes.</p>
{{else}}
<p>Enter your username. If your account has an email address on file, we'll send a reset link.</p>
{{if .Error}}<p class="banner banner-error">{{.Error}}</p>{{end}}
<form method="POST" action="/forgot-password">
<input type="text" name="username" placeholder="Username" autofocus required autocomplete="username">
<button type="submit">Send reset link</button>
</form>
{{end}}
</div>
</body>
</html>
`))

type resetWithTokenPageView struct {
	Token string
	Error string
}

var resetWithTokenPageTmpl = template.Must(template.New("reset-with-token").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Set a new password — Meeting display fleet</title>
<style>` + baseCSS + `
body { display: flex; align-items: center; justify-content: center; min-height: 100vh; padding: var(--space-5); }
.login-card { width: 100%; max-width: 22rem; padding: var(--space-6); }
.login-card .brand { margin-bottom: var(--space-5); }
form { margin-top: var(--space-5); }
input[type=password] { display: block; width: 100%; margin-bottom: var(--space-4); }
button[type=submit] { width: 100%; }
</style>
</head>
<body>
<div class="surface login-card">
` + brandMark + `
<h1>Set a new password</h1>
{{if .Error}}<p class="banner banner-error">{{.Error}}</p>{{end}}
<form method="POST" action="/reset-password">
<input type="hidden" name="token" value="{{.Token}}">
<input type="password" name="password" placeholder="New password" required autocomplete="new-password">
<button type="submit">Set password</button>
</form>
</div>
</body>
</html>
`))

// passwordResetDisabled serves a 404 rather than any page at all when the feature is off — "doesn't
// exist" is the honest answer for a deployment that hasn't provisioned a relay allowlist, not a
// link that renders and then fails.
func (s *Server) passwordResetDisabled(w http.ResponseWriter) bool {
	if s.cfg.Load().Auth.PasswordReset.Enabled {
		return false
	}
	http.NotFound(w, nil)
	return true
}

func (s *Server) handleForgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	if s.passwordResetDisabled(w) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = forgotPasswordPageTmpl.Execute(w, forgotPasswordPageView{Sent: r.URL.Query().Get("sent") != ""})
}

// handleForgotPasswordSubmit always redirects to the same "check your email" confirmation and
// takes the same amount of visible work regardless of whether the username or its email exists —
// the one easy-to-get-wrong part of this feature. It only actually mints a token and sends mail
// when both are true.
func (s *Server) handleForgotPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if s.passwordResetDisabled(w) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.PostForm.Get("username")
	host, _, err := splitRemoteAddr(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	okUser := s.forgotPasswordLimiter.allow("user:"+strings.ToLower(username), forgotPasswordRateLimit, forgotPasswordRateWindow)
	okIP := s.forgotPasswordLimiter.allow("ip:"+host, forgotPasswordRateLimit, forgotPasswordRateWindow)
	if okUser && okIP {
		if u, ok := s.cfg.Load().UserByUsername(username); ok && u.Email != "" {
			s.sendResetEmail(u)
		}
	}
	http.Redirect(w, r, "/forgot-password?sent=1", http.StatusSeeOther)
}

func (s *Server) sendResetEmail(u config.User) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		s.log.Error("reset token generation failed", "username", u.Username, "err", err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s.resetTokens.put(token, &resetToken{username: u.Username, expires: time.Now().Add(resetTokenTTL)})

	cfg := s.cfg.Load().Auth.PasswordReset
	link := strings.TrimRight(cfg.PublicBaseURL, "/") + "/reset-password?token=" + token
	body := fmt.Sprintf("A password reset was requested for your account %q.\n\n"+
		"Set a new password: %s\n\nThis link expires in 30 minutes. If you didn't request this, "+
		"you can ignore this message — your password hasn't changed.", u.Username, link)

	sender := s.emailSender
	if sender == nil {
		sender = smtpRelaySender{}
	}
	if err := sender.Send(cfg.SMTPHost, cfg.SMTPPort, cfg.FromAddress, u.Email, "Password reset — Meeting display fleet", body); err != nil {
		s.log.Error("password reset email send failed", "username", u.Username, "err", err)
	} else {
		s.log.Info("password reset email sent", "username", u.Username)
	}
}

func (s *Server) handleResetWithTokenPage(w http.ResponseWriter, r *http.Request) {
	if s.passwordResetDisabled(w) {
		return
	}
	token := r.URL.Query().Get("token")
	if _, ok := s.resetTokens.peek(token); !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resetWithTokenPageTmpl.Execute(w, resetWithTokenPageView{Error: "This reset link is invalid or has expired. Request a new one."})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = resetWithTokenPageTmpl.Execute(w, resetWithTokenPageView{Token: token})
}

func (s *Server) handleResetWithTokenSubmit(w http.ResponseWriter, r *http.Request) {
	if s.passwordResetDisabled(w) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := r.PostForm.Get("token")
	password := r.PostForm.Get("password")
	rt, ok := s.resetTokens.take(token)
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resetWithTokenPageTmpl.Execute(w, resetWithTokenPageView{Error: "This reset link is invalid or has expired. Request a new one."})
		return
	}
	if len(password) < 8 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resetWithTokenPageTmpl.Execute(w, resetWithTokenPageView{Token: token, Error: "Password must be at least 8 characters."})
		return
	}

	cfg := s.cfg.Load()
	u, ok := cfg.UserByUsername(rt.username)
	if !ok {
		http.Redirect(w, r, viewerUI.loginPath, http.StatusSeeOther)
		return
	}
	sum := sha256.Sum256([]byte(password))
	u.PasswordSHA256 = hex.EncodeToString(sum[:])
	u.MustChangePassword = false // the holder just deliberately chose this one themselves
	newCfg, err := cfg.WithUser(u)
	if err != nil {
		s.log.Error("password reset via token rejected", "username", rt.username, "err", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resetWithTokenPageTmpl.Execute(w, resetWithTokenPageView{Error: err.Error()})
		return
	}
	if err := s.applyConfig(r.Context(), newCfg, true); err != nil {
		s.log.Error("password reset via token failed to persist", "username", rt.username, "err", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resetWithTokenPageTmpl.Execute(w, resetWithTokenPageView{Error: err.Error()})
		return
	}
	s.sessions.RevokeByUsername(rt.username)
	s.log.Info("password reset via emailed token", "username", rt.username)
	http.Redirect(w, r, viewerUI.loginPath+"?reset=1", http.StatusSeeOther)
}

// splitRemoteAddr is a tiny local host/port split so the rate limiter keys on an IP, not
// "ip:port" (which would make every request from behind the same NAT count as a different key).
func splitRemoteAddr(remoteAddr string) (host string, port string, err error) {
	i := strings.LastIndex(remoteAddr, ":")
	if i < 0 {
		return remoteAddr, "", nil
	}
	return remoteAddr[:i], remoteAddr[i+1:], nil
}
