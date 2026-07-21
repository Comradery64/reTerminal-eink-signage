package server

import (
	"html/template"
	"net/http"
	"net/url"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
)

// roleUI bundles the per-role paths/cookie name a login flow needs. Every role is independently
// configurable and independently gated — logging into one never grants another, enforced by
// using a separate cookie scoped to each role's own path.
type roleUI struct {
	role       auth.Role
	label      string // display form of role, e.g. "Admin" — used in the sign-in heading
	cookieName string
	loginPath  string
	homePath   string
}

var (
	adminUI        = roleUI{role: auth.RoleAdmin, label: "Admin", cookieName: "admin_session", loginPath: "/admin/login", homePath: "/admin"}
	managerUI      = roleUI{role: auth.RoleManager, label: "Manager", cookieName: "manager_session", loginPath: "/manager/login", homePath: "/manager"}
	receptionistUI = roleUI{role: auth.RoleReceptionist, label: "Receptionist", cookieName: "receptionist_session", loginPath: "/receptionist/login", homePath: "/receptionist"}
)

// requireRole gates next behind a valid session cookie for ui.role, redirecting to that role's
// login page otherwise. Mirrors deviceAuth.verify's gating of handleDisplay/handleTelemetry, just
// against a session cookie instead of a bearer header.
func (s *Server) requireRole(ui roleUI, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(ui.cookieName)
		if err != nil {
			http.Redirect(w, r, ui.loginPath+"?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
			return
		}
		role, ok := s.sessions.Check(c.Value)
		if !ok || role != ui.role {
			http.Redirect(w, r, ui.loginPath, http.StatusSeeOther)
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
		role, ok := s.userDirectory().Verify(username, password)
		if !ok || role != ui.role {
			redirect := ui.loginPath + "?error=1"
			if next := r.PostForm.Get("next"); next != "" {
				redirect += "&next=" + url.QueryEscape(next)
			}
			http.Redirect(w, r, redirect, http.StatusSeeOther)
			return
		}

		token := s.sessions.Create(ui.role)
		http.SetCookie(w, &http.Cookie{
			Name:     ui.cookieName,
			Value:    token,
			Path:     ui.homePath,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})

		dest := ui.homePath
		if next := r.PostForm.Get("next"); next != "" {
			dest = next
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
