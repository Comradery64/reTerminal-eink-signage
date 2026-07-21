package server

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
)

// roleUI bundles the per-role paths/cookie name a login flow needs. Every role is independently
// configurable and independently gated — logging into one never grants another, enforced by
// using a separate cookie scoped to each role's own path.
type roleUI struct {
	role               auth.Role
	label              string // display form of role, e.g. "Admin" — used in the sign-in heading
	cookieName         string
	loginPath          string
	homePath           string
	changePasswordPath string
	totpVerifyPath     string
}

var (
	adminUI   = roleUI{role: auth.RoleAdmin, label: "Admin", cookieName: "admin_session", loginPath: "/admin/login", homePath: "/admin", changePasswordPath: "/admin/change-password", totpVerifyPath: "/admin/verify-2fa"}
	managerUI = roleUI{role: auth.RoleManager, label: "Manager", cookieName: "manager_session", loginPath: "/manager/login", homePath: "/manager", changePasswordPath: "/manager/change-password", totpVerifyPath: "/manager/verify-2fa"}
	viewerUI  = roleUI{role: auth.RoleViewer, label: "Viewer", cookieName: "viewer_session", loginPath: "/dashboard/login", homePath: "/dashboard", changePasswordPath: "/dashboard/change-password", totpVerifyPath: "/dashboard/verify-2fa"}
)

// requireRole gates next behind a valid session cookie for ui.role, redirecting to that role's
// login page otherwise. Mirrors deviceAuth.verify's gating of handleDisplay/handleTelemetry, just
// against a session cookie instead of a bearer header.
//
// Two session flags can redirect elsewhere instead of next, checked in this order — both are
// resolved the same way: every request except the one page that can clear the flag bounces there
// until it's cleared, then the session is reissued without it.
//   - MustChangePassword (set by an admin granting/resetting this account — see
//     handleAdminSaveUser): the account can't be used for anything until the holder replaces the
//     admin-chosen password with one only they know.
//   - Pending2FA (set when a TOTP-enrolled account passes the password check but hasn't yet
//     entered its second factor for this login — see handleLoginSubmit/handleTOTPVerifySubmit).
func (s *Server) requireRole(ui roleUI, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(ui.cookieName)
		if err != nil {
			http.Redirect(w, r, ui.loginPath+"?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
			return
		}
		sess, ok := s.sessions.Check(c.Value)
		if !ok || sess.Role != ui.role {
			http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
			return
		}
		if sess.MustChangePassword && r.URL.Path != ui.changePasswordPath {
			http.Redirect(w, r, ui.changePasswordPath, http.StatusSeeOther)
			return
		}
		if sess.Pending2FA && r.URL.Path != ui.totpVerifyPath {
			http.Redirect(w, r, ui.totpVerifyPath, http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLoginPage(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = loginPageTmpl.Execute(w, loginPageView{
			RoleLabel: ui.label,
			Path:      ui.loginPath,
			Next:      r.URL.Query().Get("next"),
			Error:     r.URL.Query().Get("error") != "",
		})
	}
}

func (s *Server) handleLoginSubmit(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		username := r.PostForm.Get("username")
		password := r.PostForm.Get("password")
		// Reject a correct login at the wrong door with the same generic message as a wrong
		// password — telling the user "your account exists but doesn't work here" would leak
		// which usernames are valid.
		result, ok := s.userDirectory().Verify(username, password)
		if !ok || result.Role != ui.role {
			redirect := ui.loginPath + "?error=1"
			if next := r.PostForm.Get("next"); next != "" {
				redirect += "&next=" + url.QueryEscape(next)
			}
			http.Redirect(w, r, redirect, http.StatusSeeOther)
			return
		}

		flags := auth.SessionFlags{MustChangePassword: result.MustChangePassword, Pending2FA: result.TOTPEnabled}
		token := s.sessions.Create(ui.role, username, flags)
		http.SetCookie(w, &http.Cookie{
			Name:     ui.cookieName,
			Value:    token,
			Path:     ui.homePath,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})

		// MustChangePassword takes priority — an admin-chosen temporary password is resolved
		// before anything else, including the second factor (handleChangePasswordSubmit reissues
		// the session preserving Pending2FA, so a 2FA-enrolled account still verifies it next).
		dest := ui.homePath
		switch {
		case result.MustChangePassword:
			dest = ui.changePasswordPath
		case result.TOTPEnabled:
			dest = ui.totpVerifyPath
		case r.PostForm.Get("next") != "":
			dest = r.PostForm.Get("next")
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

func (s *Server) handleLogout(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(ui.cookieName); err == nil {
			s.sessions.Revoke(c.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     ui.cookieName,
			Value:    "",
			Path:     ui.homePath,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
	}
}

// handleChangePasswordPage renders the form a MustChangePassword session is redirected to by
// requireRole. It's the only page such a session can reach until the password is replaced.
func (s *Server) handleChangePasswordPage(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = changePasswordPageTmpl.Execute(w, changePasswordPageView{
			RoleLabel: ui.label,
			Path:      ui.changePasswordPath,
			Error:     r.URL.Query().Get("error") != "",
		})
	}
}

// handleChangePasswordSubmit sets a new password for the session's own account (looked up
// server-side from the session token, never from a client-supplied field, so this session can
// only ever change its own account's password) and clears MustChangePassword. The old session
// token is revoked and replaced with a fresh one, so the account lands straight on its home page
// without a second login.
func (s *Server) handleChangePasswordSubmit(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(ui.cookieName)
		if err != nil {
			http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
			return
		}
		sess, ok := s.sessions.Check(c.Value)
		if !ok || sess.Role != ui.role {
			http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		newPassword := r.PostForm.Get("password")
		confirm := r.PostForm.Get("password_confirm")
		if newPassword == "" || newPassword != confirm {
			http.Redirect(w, r, ui.changePasswordPath+"?error=1", http.StatusSeeOther)
			return
		}

		existing, ok := s.cfg.Load().UserByUsername(sess.Username)
		if !ok {
			// The account was deleted from under an active session — nothing to change.
			http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
			return
		}
		sum := sha256.Sum256([]byte(newPassword))
		existing.PasswordSHA256 = hex.EncodeToString(sum[:])
		existing.MustChangePassword = false
		newCfg, err := s.cfg.Load().WithUser(existing)
		if err != nil {
			s.log.Error("change password rejected", "username", sess.Username, "err", err)
			http.Redirect(w, r, ui.changePasswordPath+"?error=1", http.StatusSeeOther)
			return
		}
		s.applyConfig(newCfg, true)

		// Preserve Pending2FA from the old session — a 2FA-enrolled account whose password was
		// just reset still has to verify its second factor next, not skip straight to homePath.
		s.sessions.Revoke(c.Value)
		token := s.sessions.Create(ui.role, sess.Username, auth.SessionFlags{Pending2FA: sess.Pending2FA})
		http.SetCookie(w, &http.Cookie{
			Name:     ui.cookieName,
			Value:    token,
			Path:     ui.homePath,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		dest := ui.homePath
		if sess.Pending2FA {
			dest = ui.totpVerifyPath
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

// handleTOTPVerifyPage renders the code-entry form a Pending2FA session is redirected to by
// requireRole. It's the only page such a session can reach until the code is verified.
func (s *Server) handleTOTPVerifyPage(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = totpVerifyPageTmpl.Execute(w, totpPageView{
			RoleLabel: ui.label,
			Path:      ui.totpVerifyPath,
			Error:     r.URL.Query().Get("error") != "",
		})
	}
}

// handleTOTPVerifySubmit checks the submitted code against the session's own account's secret
// (looked up server-side, same pattern as handleChangePasswordSubmit) and, if it matches, reissues
// the session with Pending2FA cleared.
func (s *Server) handleTOTPVerifySubmit(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(ui.cookieName)
		if err != nil {
			http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
			return
		}
		sess, ok := s.sessions.Check(c.Value)
		if !ok || sess.Role != ui.role {
			http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		existing, ok := s.cfg.Load().UserByUsername(sess.Username)
		if !ok || existing.TOTPSecret == "" || !auth.VerifyTOTP(existing.TOTPSecret, r.PostForm.Get("code"), time.Now()) {
			http.Redirect(w, r, ui.totpVerifyPath+"?error=1", http.StatusSeeOther)
			return
		}

		s.sessions.Revoke(c.Value)
		token := s.sessions.Create(ui.role, sess.Username, auth.SessionFlags{})
		http.SetCookie(w, &http.Cookie{
			Name:     ui.cookieName,
			Value:    token,
			Path:     ui.homePath,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, ui.homePath, http.StatusSeeOther)
	}
}

type totpPageView struct {
	RoleLabel string
	Path      string
	Error     bool
}

var totpVerifyPageTmpl = template.Must(template.New("verify-2fa").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Verify — Meeting display fleet</title>
<style>` + baseCSS + `
body { display: flex; align-items: center; justify-content: center; min-height: 100vh; padding: var(--space-5); }
.login-card { width: 100%; max-width: 22rem; padding: var(--space-6); }
.login-card .brand { margin-bottom: var(--space-5); }
form { margin-top: var(--space-5); }
input[type=text] { display: block; width: 100%; margin-bottom: var(--space-4); font-family: var(--font-mono); letter-spacing: .1em; text-align: center; }
button[type=submit] { width: 100%; }
</style>
</head>
<body>
<div class="surface login-card">
` + brandMark + `
<h1>Enter your code</h1>
<p>Enter the 6-digit code from your authenticator app.</p>
{{if .Error}}<p class="banner banner-error" style="margin:var(--space-3) 0 0">Incorrect or expired code.</p>{{end}}
<form method="POST" action="{{.Path}}">
<input type="text" name="code" placeholder="000000" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autofocus required autocomplete="one-time-code">
<button type="submit">Verify</button>
</form>
</div>
</body>
</html>
`))

type changePasswordPageView struct {
	RoleLabel string
	Path      string
	Error     bool
}

var changePasswordPageTmpl = template.Must(template.New("change-password").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Choose a password — Meeting display fleet</title>
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
<h1>Choose a password</h1>
<p>Your {{.RoleLabel}} account was just granted or reset with a temporary password. Pick a new one only you know before continuing.</p>
{{if .Error}}<p class="banner banner-error" style="margin:var(--space-3) 0 0">Passwords must match and can't be blank.</p>{{end}}
<form method="POST" action="{{.Path}}">
<input type="password" name="password" placeholder="New password" autofocus required autocomplete="new-password">
<input type="password" name="password_confirm" placeholder="Confirm new password" required autocomplete="new-password">
<button type="submit">Set password</button>
</form>
</div>
</body>
</html>
`))

type loginPageView struct {
	RoleLabel string
	Path      string
	Next      string
	Error     bool
}

var loginPageTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.RoleLabel}} sign-in — Meeting display fleet</title>
<style>` + baseCSS + `
body { display: flex; align-items: center; justify-content: center; min-height: 100vh; padding: var(--space-5); }
.login-card { width: 100%; max-width: 22rem; padding: var(--space-6); }
.login-card .brand { margin-bottom: var(--space-5); }
form { margin-top: var(--space-5); }
input[type=text], input[type=password] { display: block; width: 100%; margin-bottom: var(--space-4); }
button[type=submit] { width: 100%; }
</style>
</head>
<body>
<div class="surface login-card">
` + brandMark + `
<h1>{{.RoleLabel}} sign-in</h1>
<p>Sign in with your assigned account.</p>
{{if .Error}}<p class="banner banner-error" style="margin:var(--space-3) 0 0">Incorrect username or password.</p>{{end}}
<form method="POST" action="{{.Path}}">
<input type="text" name="username" placeholder="Username" autofocus required autocomplete="username">
<input type="password" name="password" placeholder="Password" required autocomplete="current-password">
<input type="hidden" name="next" value="{{.Next}}">
<button type="submit">Log in</button>
</form>
</div>
</body>
</html>
`))
