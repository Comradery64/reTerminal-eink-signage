package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/admin"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	data := adminPageData{
		View:         admin.Build(cfg),
		Error:        r.URL.Query().Get("error"),
		SavedSection: r.URL.Query().Get("saved"),
	}
	// Two-factor status is about the CURRENT session's own account, not something one admin sets
	// for another — pulled straight from cfg, same as any other per-account field.
	if c, err := r.Cookie(adminUI.cookieName); err == nil {
		if sess, ok := s.sessions.Check(c.Value); ok {
			data.Username = sess.Username
			if u, ok := cfg.UserByUsername(sess.Username); ok {
				data.TOTPEnabled = u.TOTPSecret != ""
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminPageTmpl.Execute(w, data); err != nil {
		s.log.Error("admin page render failed", "err", err)
	}
}

// handleAdminSaveRoom handles both add (empty original_device_id) and edit (non-empty). The token
// field is optional on edit — leave it blank to keep the room's existing token; a room being
// added for the first time must supply one.
func (s *Server) handleAdminSaveRoom(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cfg := s.cfg.Load()
	deviceID := r.PostForm.Get("device_id")
	originalDeviceID := r.PostForm.Get("original_device_id")

	tokenHash := ""
	if plaintext := r.PostForm.Get("token"); plaintext != "" {
		sum := sha256.Sum256([]byte(plaintext))
		tokenHash = hex.EncodeToString(sum[:])
	} else if originalDeviceID != "" {
		if existing, ok := cfg.RoomByDeviceID(originalDeviceID); ok {
			tokenHash = existing.TokenSHA256
		}
	}

	room := config.Room{
		DeviceID:    deviceID,
		Name:        r.PostForm.Get("name"),
		Room:        r.PostForm.Get("room"),
		TokenSHA256: tokenHash,
	}

	newCfg, err := cfg.WithRoom(room)
	if err != nil {
		s.log.Error("admin room save rejected", "device", deviceID, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	// A room rename (device_id changed) needs the old entry removed too.
	if originalDeviceID != "" && originalDeviceID != deviceID {
		if withoutOld, err := newCfg.WithoutRoom(originalDeviceID); err == nil {
			newCfg = withoutOld
		}
	}
	s.applyConfig(newCfg, true) // tokens/names changed — rebuild derived caches
	http.Redirect(w, r, "/admin?saved=rooms", http.StatusSeeOther)
}

func (s *Server) handleAdminDeleteRoom(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	deviceID := r.PostForm.Get("device_id")
	newCfg, err := s.cfg.Load().WithoutRoom(deviceID)
	if err != nil {
		s.log.Error("admin room delete rejected", "device", deviceID, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, true)
	http.Redirect(w, r, "/admin?saved=rooms", http.StatusSeeOther)
}

func (s *Server) handleAdminSaveWakeDefaults(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	wake := s.cfg.Load().Wake // start from current, only override the fields this form owns
	wake.Mode = r.PostForm.Get("mode")
	wake.Timezone = r.PostForm.Get("timezone")
	wake.BusinessHoursSeconds = formUint32(r, "business_hours_seconds")
	wake.OffHoursSeconds = formUint32(r, "off_hours_seconds")
	wake.BusinessStartHour = formInt(r, "business_start_hour")
	wake.BusinessEndHour = formInt(r, "business_end_hour")
	if raw := r.PostForm.Get("forced_refresh_hour"); raw != "" {
		h := formInt(r, "forced_refresh_hour")
		wake.ForcedRefreshHour = &h
	} else {
		wake.ForcedRefreshHour = nil
	}

	newCfg, err := s.cfg.Load().WithWake(wake)
	if err != nil {
		s.log.Error("admin wake defaults save rejected", "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, false)
	http.Redirect(w, r, "/admin?saved=wake", http.StatusSeeOther)
}

func (s *Server) handleAdminSaveAlerts(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	alerts := config.AlertConfig{
		LowBatteryPct: formInt(r, "low_battery_pct"),
		ClearPct:      formInt(r, "clear_pct"),
		MinRenotify:   formDuration(r, "min_renotify"),
		StaleAfter:    formDuration(r, "stale_after"),
		WebhookURL:    r.PostForm.Get("webhook_url"),
	}
	newCfg, err := s.cfg.Load().WithAlerts(alerts)
	if err != nil {
		s.log.Error("admin alerts save rejected", "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, false)
	http.Redirect(w, r, "/admin?saved=alerts", http.StatusSeeOther)
}

func (s *Server) handleAdminSaveFirmware(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	fw := config.FirmwareConfig{
		Version: r.PostForm.Get("version"),
		URL:     r.PostForm.Get("url"),
		Dir:     r.PostForm.Get("dir"),
	}
	newCfg, err := s.cfg.Load().WithFirmware(fw)
	if err != nil {
		s.log.Error("admin firmware save rejected", "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, false)
	http.Redirect(w, r, "/admin?saved=firmware", http.StatusSeeOther)
}

// handleAdminSaveUser handles both add (new username) and edit (existing username) — this is how
// an admin grants or changes an employee's access level. The password field is optional on edit;
// leave it blank to keep the account's existing password while changing only its role.
func (s *Server) handleAdminSaveUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cfg := s.cfg.Load()
	username := r.PostForm.Get("username")
	originalUsername := r.PostForm.Get("original_username")

	// Any password an admin types here is a temporary one the account holder never chose — force
	// them to replace it with something only they know before the account can do anything else
	// (see requireRole/handleChangePasswordSubmit). Leaving the field blank on an edit keeps the
	// existing password AND its existing MustChangePassword state untouched.
	passwordHash := ""
	mustChangePassword := false
	if plaintext := r.PostForm.Get("password"); plaintext != "" {
		sum := sha256.Sum256([]byte(plaintext))
		passwordHash = hex.EncodeToString(sum[:])
		mustChangePassword = true
	} else if originalUsername != "" {
		if existing, ok := cfg.UserByUsername(originalUsername); ok {
			passwordHash = existing.PasswordSHA256
			mustChangePassword = existing.MustChangePassword
		}
	}

	user := config.User{
		Username:           username,
		PasswordSHA256:     passwordHash,
		Role:               r.PostForm.Get("role"),
		MustChangePassword: mustChangePassword,
	}

	newCfg, err := cfg.WithUser(user)
	if err != nil {
		s.log.Error("admin user save rejected", "username", username, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error())+"#access", http.StatusSeeOther)
		return
	}
	// A username rename needs the old entry removed too (mirrors handleAdminSaveRoom's rename path).
	if originalUsername != "" && !strings.EqualFold(originalUsername, username) {
		if withoutOld, err := newCfg.WithoutUser(originalUsername); err == nil {
			newCfg = withoutOld
		}
	}
	s.applyConfig(newCfg, true) // login directory changed — rebuild it immediately
	http.Redirect(w, r, "/admin?saved=access", http.StatusSeeOther)
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.PostForm.Get("username")
	newCfg, err := s.cfg.Load().WithoutUser(username)
	if err != nil {
		s.log.Error("admin user delete rejected", "username", username, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error())+"#access", http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, true)
	http.Redirect(w, r, "/admin?saved=access", http.StatusSeeOther)
}

// currentAdminSession returns the session bound to r's admin_session cookie. Self-service account
// handlers (2FA setup/enable/disable) use this — never a client-supplied username — so a request
// can only ever act on the account that's actually logged in.
func (s *Server) currentAdminSession(r *http.Request) (auth.Session, bool) {
	c, err := r.Cookie(adminUI.cookieName)
	if err != nil {
		return auth.Session{}, false
	}
	sess, ok := s.sessions.Check(c.Value)
	if !ok || sess.Role != adminUI.role {
		return auth.Session{}, false
	}
	return sess, true
}

// totpSetupQRDataURI renders uri as a QR code PNG and returns it as a data: URI, so the setup
// page can embed it directly with no extra request — and no route that would otherwise need the
// secret in a query string to serve the image.
func totpSetupQRDataURI(uri string) string {
	png, err := qrcode.Encode(uri, qrcode.Medium, 220)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// handleTOTPSetupPage generates a fresh secret (not yet saved anywhere) and shows it — as a
// scannable QR code plus the plain-text secret and otpauth:// URI as a fallback — for the account
// holder to add to their authenticator app before confirming with a code.
func (s *Server) handleTOTPSetupPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentAdminSession(r)
	if !ok {
		http.Redirect(w, r, adminUI.loginPath, http.StatusSeeOther)
		return
	}
	secret := auth.NewTOTPSecret()
	uri := auth.TOTPURI("MeetingDisplayFleet", sess.Username, secret)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = totpSetupPageTmpl.Execute(w, totpSetupPageView{
		Username: sess.Username,
		Secret:   secret,
		URI:      uri,
		QRImage:  template.URL(totpSetupQRDataURI(uri)),
	})
}

// handleTOTPEnableSubmit confirms enrollment: the secret travels back in a hidden form field (safe
// here — it's the account holder's own browser confirming a secret they just saw, not a
// client able to affect any other account) and is only ever persisted once a real code from it
// verifies. On a wrong code, redisplays the SAME secret directly (not a redirect) so it never
// appears in a URL or a server log.
func (s *Server) handleTOTPEnableSubmit(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentAdminSession(r)
	if !ok {
		http.Redirect(w, r, adminUI.loginPath, http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	secret := r.PostForm.Get("secret")
	if !auth.VerifyTOTP(secret, r.PostForm.Get("code"), time.Now()) {
		uri := auth.TOTPURI("MeetingDisplayFleet", sess.Username, secret)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = totpSetupPageTmpl.Execute(w, totpSetupPageView{
			Username: sess.Username,
			Secret:   secret,
			URI:      uri,
			QRImage:  template.URL(totpSetupQRDataURI(uri)),
			Error:    true,
		})
		return
	}

	existing, ok := s.cfg.Load().UserByUsername(sess.Username)
	if !ok {
		http.Redirect(w, r, adminUI.loginPath, http.StatusSeeOther)
		return
	}
	existing.TOTPSecret = secret
	newCfg, err := s.cfg.Load().WithUser(existing)
	if err != nil {
		s.log.Error("2fa enable rejected", "username", sess.Username, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error())+"#security", http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, true)
	http.Redirect(w, r, "/admin?saved=security#security", http.StatusSeeOther)
}

// handleTOTPDisableSubmit requires a current code (not just an active session) before turning 2FA
// off — a stolen session cookie alone shouldn't be enough to downgrade the account's own defenses.
func (s *Server) handleTOTPDisableSubmit(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentAdminSession(r)
	if !ok {
		http.Redirect(w, r, adminUI.loginPath, http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	existing, ok := s.cfg.Load().UserByUsername(sess.Username)
	if !ok || existing.TOTPSecret == "" || !auth.VerifyTOTP(existing.TOTPSecret, r.PostForm.Get("code"), time.Now()) {
		http.Redirect(w, r, "/admin?error=incorrect+code#security", http.StatusSeeOther)
		return
	}
	existing.TOTPSecret = ""
	newCfg, err := s.cfg.Load().WithUser(existing)
	if err != nil {
		s.log.Error("2fa disable rejected", "username", sess.Username, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error())+"#security", http.StatusSeeOther)
		return
	}
	s.applyConfig(newCfg, true)
	http.Redirect(w, r, "/admin?saved=security#security", http.StatusSeeOther)
}

type totpSetupPageView struct {
	Username string
	Secret   string
	URI      string
	// QRImage is a data: URI — template.URL marks it pre-vetted-safe so html/template's
	// contextual auto-escaper doesn't rewrite it to "#ZgotmplZ" (its default for any URL scheme
	// besides http(s)/mailto). Safe here because we generate the whole string ourselves from a
	// server-side secret; nothing in it is attacker-controlled.
	QRImage template.URL // empty if QR encoding failed (falls back to secret/URI text only)
	Error   bool
}

var totpSetupPageTmpl = template.Must(template.New("2fa-setup").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Enable two-factor authentication — Meeting display fleet</title>
<style>` + baseCSS + `
body { display: flex; align-items: center; justify-content: center; min-height: 100vh; padding: var(--space-5); }
.login-card { width: 100%; max-width: 26rem; padding: var(--space-6); }
.login-card .brand { margin-bottom: var(--space-5); }
.qr { display: block; margin: 0 auto var(--space-4); border: 1px solid var(--line); border-radius: var(--radius); }
.secret { font-family: var(--font-mono); font-size: var(--text-md); letter-spacing: .05em; word-break: break-all; padding: var(--space-3); background: var(--paper); border: 1px solid var(--line); border-radius: var(--radius); margin: var(--space-2) 0 var(--space-4); }
.uri { font-family: var(--font-mono); font-size: var(--text-xs); word-break: break-all; color: var(--ink-soft); margin-bottom: var(--space-4); }
label { display: block; font-size: var(--text-sm); font-weight: 600; color: var(--ink-soft); margin: var(--space-3) 0 var(--space-2); text-transform: uppercase; letter-spacing: .03em; }
form { margin-top: var(--space-4); }
input[type=text] { display: block; width: 100%; margin-bottom: var(--space-4); font-family: var(--font-mono); letter-spacing: .1em; text-align: center; }
button[type=submit] { width: 100%; }
</style>
</head>
<body>
<div class="surface login-card">
` + brandMark + `
<h1>Enable two-factor authentication</h1>
<p>Scan this with your authenticator app (or enter the setup key below if it can't scan) to add
this account ({{.Username}}).</p>
{{if .QRImage}}<img class="qr" src="{{.QRImage}}" width="220" height="220" alt="Scannable QR code for this account's 2FA setup key">{{end}}
<div class="secret">{{.Secret}}</div>
<div class="uri">{{.URI}}</div>
{{if .Error}}<p class="banner banner-error">Incorrect or expired code — try again.</p>{{end}}
<form method="POST" action="/admin/2fa/enable">
<input type="hidden" name="secret" value="{{.Secret}}">
<label for="enable-code">Enter the current 6-digit code to confirm</label>
<input id="enable-code" type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autofocus required autocomplete="one-time-code">
<button type="submit">Enable 2FA</button>
</form>
</div>
</body>
</html>
`))

func formInt(r *http.Request, key string) int {
	v, _ := strconv.Atoi(r.PostForm.Get(key))
	return v
}

func formUint32(r *http.Request, key string) uint32 {
	v, _ := strconv.ParseUint(r.PostForm.Get(key), 10, 32)
	return uint32(v)
}

func formDuration(r *http.Request, key string) time.Duration {
	d, _ := time.ParseDuration(r.PostForm.Get(key))
	return d
}

type adminPageData struct {
	View         admin.View
	Error        string
	SavedSection string // e.g. "rooms" — which section to flash, empty on a plain page load
	Username     string // current session's own account — 2FA is self-service, not admin-on-admin
	TOTPEnabled  bool
}

var adminPageTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Meeting display fleet — admin</title>
<style>` + baseCSS + `
.layout { display: flex; min-height: 100vh; }
.rail {
  flex: 0 0 12rem; padding: var(--space-5) var(--space-4); border-right: 1px solid var(--line);
  position: sticky; top: 0; align-self: flex-start; height: 100vh;
  display: flex; flex-direction: column;
}
.rail .brand { margin-bottom: var(--space-5); }
.rail .links { display: flex; flex-direction: column; gap: var(--space-1); }
.rail a {
  display: block; padding: var(--space-2) var(--space-3); border-radius: var(--radius);
  color: var(--ink-soft); text-decoration: none; font-size: var(--text-sm); font-weight: 600;
}
.rail a:hover { background: var(--paper-raised); color: var(--ink); }
.rail form { margin-top: auto; }
.main { flex: 1; padding: var(--space-5) var(--space-7) var(--space-7); max-width: 52rem; }
.section { padding-top: var(--space-5); border-top: 1px solid var(--line); margin-top: var(--space-5); }
.section:first-of-type { margin-top: 0; border-top: none; padding-top: 0; }
table { border-collapse: collapse; width: 100%; margin-bottom: var(--space-4); }
th, td { border-bottom: 1px solid var(--line); padding: var(--space-2) var(--space-3); text-align: left; font-size: var(--text-base); vertical-align: middle; }
th { font-family: var(--font-mono); font-size: var(--text-xs); text-transform: uppercase; letter-spacing: .05em; color: var(--ink-soft); }
td.mono { font-family: var(--font-mono); font-size: var(--text-sm); }
label { display: block; font-size: var(--text-sm); font-weight: 600; color: var(--ink-soft); margin: var(--space-3) 0 var(--space-1); text-transform: uppercase; letter-spacing: .03em; }
input:not([type=submit]), select { display: block; width: 100%; max-width: 22rem; }
form button[type=submit]:not(.danger):not(.ghost) { margin-top: var(--space-4); }
.add-room { margin-top: var(--space-4); padding: var(--space-4) var(--space-5); width: fit-content; }
.add-room summary { cursor: pointer; font-weight: 600; color: var(--blue); }
.add-room[open] summary { margin-bottom: var(--space-3); }
@media (max-width: 46rem) {
  .layout { flex-direction: column; }
  .rail { position: static; height: auto; width: 100%; flex: none; flex-direction: row; align-items: center; border-right: none; border-bottom: 1px solid var(--line); flex-wrap: wrap; gap: var(--space-3); }
  .rail .brand { margin-bottom: 0; }
  .rail .links { flex-direction: row; }
  .rail form { margin-top: 0; margin-left: auto; }
  .main { padding: var(--space-4) var(--space-4) var(--space-6); max-width: none; }
}
</style>
</head>
<body>
<div class="layout">
<nav class="rail">
` + brandMark + `
<div class="links">
<a href="#rooms">Rooms</a>
<a href="#access">Access</a>
<a href="#security">Security</a>
<a href="#wake">Wake defaults</a>
<a href="#alerts">Alerts</a>
<a href="#firmware">Firmware</a>
</div>
<form method="POST" action="/admin/logout"><button type="submit" class="ghost">Log out</button></form>
</nav>
<main class="main">
{{if .Error}}<p class="banner banner-error" style="margin-left:0;margin-right:0">{{.Error}}</p>{{end}}

<section class="section {{if eq .SavedSection "rooms"}}flash{{end}}" id="rooms">
<h2>Rooms</h2>
<table>
<tr><th>Device</th><th>Name</th><th>Calendar</th><th>Wake override</th><th>Token</th><th></th></tr>
{{range .View.Rooms}}
<tr>
<td class="mono">{{.DeviceID}}</td><td>{{.Name}}</td><td class="mono">{{.Room}}</td>
<td>{{if .WakeMode}}{{.WakeMode}} ({{.FlatIntervalSeconds}}s){{else}}fleet default{{end}}</td>
<td>{{if .TokenConfigured}}configured{{else}}<em>none</em>{{end}}</td>
<td><form method="POST" action="/admin/rooms/delete" style="display:inline">
<input type="hidden" name="device_id" value="{{.DeviceID}}">
<button type="submit" class="danger" onclick="return confirm('Remove {{.Name}} from the fleet? This can\'t be undone here.')">Remove</button>
</form></td>
</tr>
{{end}}
</table>
<details class="surface add-room">
<summary>Add or edit a room</summary>
<form method="POST" action="/admin/rooms/save">
<input type="hidden" name="original_device_id" value="">
<label for="r-device">Device ID</label><input id="r-device" type="text" name="device_id" required>
<label for="r-name">Name</label><input id="r-name" type="text" name="name" required>
<label for="r-room">Calendar (room email)</label><input id="r-room" type="text" name="room" required>
<label for="r-token">Device token (leave blank to keep existing)</label><input id="r-token" type="text" name="token">
<button type="submit">Save room</button>
</form>
</details>
</section>

<section class="section {{if eq .SavedSection "access"}}flash{{end}}" id="access">
<h2>Access</h2>
<p>Grant or revoke an employee's login to /admin, /manager, or /dashboard. Role controls what
they can see and do — manager gets status plus wake-mode control, viewer gets status only.</p>
<table>
<tr><th>Username</th><th>Role</th><th></th></tr>
{{range .View.Users}}
<tr>
<td class="mono">{{.Username}}</td><td>{{.Role}}</td>
<td><form method="POST" action="/admin/access/delete" style="display:inline">
<input type="hidden" name="username" value="{{.Username}}">
<button type="submit" class="danger" onclick="return confirm('Revoke access for {{.Username}}?')">Revoke</button>
</form></td>
</tr>
{{end}}
</table>
<details class="surface add-room">
<summary>Grant or edit access</summary>
<form method="POST" action="/admin/access/save">
<input type="hidden" name="original_username" value="">
<label for="u-username">Username</label><input id="u-username" type="text" name="username" required>
<label for="u-role">Role</label>
<select id="u-role" name="role">
<option value="admin">Admin — full config access</option>
<option value="manager" selected>Manager — status + wake-mode control</option>
<option value="viewer">Viewer — status only, read-only</option>
</select>
<label for="u-password">Password (leave blank to keep existing)</label><input id="u-password" type="text" name="password">
<button type="submit">Save access</button>
</form>
</details>
</section>

<section class="section {{if eq .SavedSection "security"}}flash{{end}}" id="security">
<h2>Security</h2>
<p>Two-factor authentication for your own account ({{.Username}}) — one admin can never see or set
another admin's code, each account enrolls its own authenticator app.</p>
{{if .TOTPEnabled}}
<p class="chip chip-ok">Enabled</p>
<details class="surface add-room">
<summary>Disable two-factor authentication</summary>
<form method="POST" action="/admin/2fa/disable">
<label for="disable-code">Enter a current code to confirm</label>
<input id="disable-code" type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required autocomplete="one-time-code">
<button type="submit" class="danger">Disable 2FA</button>
</form>
</details>
{{else}}
<p class="chip chip-warn">Not enabled</p>
<p><a href="/admin/2fa/setup">Enable two-factor authentication</a></p>
{{end}}
</section>

<section class="section {{if eq .SavedSection "wake"}}flash{{end}}" id="wake">
<h2>Wake defaults</h2>
<p>Applies fleet-wide unless a room has its own override.</p>
<form method="POST" action="/admin/wake/save">
<label for="w-mode">Mode</label>
<select id="w-mode" name="mode">
<option value="flat" {{if ne .View.Wake.Mode "smart"}}selected{{end}}>Flat — fixed interval</option>
<option value="smart" {{if eq .View.Wake.Mode "smart"}}selected{{end}}>Smart — calendar-driven</option>
</select>
<label for="w-tz">Timezone</label><input id="w-tz" type="text" name="timezone" value="{{.View.Wake.Timezone}}">
<label for="w-bh">Business hours interval (seconds)</label><input id="w-bh" type="number" name="business_hours_seconds" value="{{.View.Wake.BusinessHoursSeconds}}">
<label for="w-oh">Off hours interval (seconds)</label><input id="w-oh" type="number" name="off_hours_seconds" value="{{.View.Wake.OffHoursSeconds}}">
<label for="w-bs">Business start hour</label><input id="w-bs" type="number" name="business_start_hour" value="{{.View.Wake.BusinessStartHour}}">
<label for="w-be">Business end hour</label><input id="w-be" type="number" name="business_end_hour" value="{{.View.Wake.BusinessEndHour}}">
<label for="w-fr">Forced daily refresh hour (blank disables)</label><input id="w-fr" type="number" name="forced_refresh_hour" value="{{if .View.Wake.ForcedRefreshHour}}{{.View.Wake.ForcedRefreshHour}}{{end}}">
<button type="submit">Save wake defaults</button>
</form>
</section>

<section class="section {{if eq .SavedSection "alerts"}}flash{{end}}" id="alerts">
<h2>Alerts</h2>
<form method="POST" action="/admin/alerts/save">
<label for="a-low">Low battery threshold (%)</label><input id="a-low" type="number" name="low_battery_pct" value="{{.View.Alerts.LowBatteryPct}}">
<label for="a-clear">Clear threshold (%)</label><input id="a-clear" type="number" name="clear_pct" value="{{.View.Alerts.ClearPct}}">
<label for="a-renotify">Minimum time between repeat alerts</label><input id="a-renotify" type="text" name="min_renotify" value="{{.View.Alerts.MinRenotify}}" placeholder="e.g. 24h">
<label for="a-stale">Mark stale after</label><input id="a-stale" type="text" name="stale_after" value="{{.View.Alerts.StaleAfter}}" placeholder="e.g. 1h">
<label for="a-webhook">Webhook URL</label><input id="a-webhook" type="text" name="webhook_url" value="{{.View.Alerts.WebhookURL}}">
<button type="submit">Save alerts</button>
</form>
</section>

<section class="section {{if eq .SavedSection "firmware"}}flash{{end}}" id="firmware">
<h2>Firmware OTA</h2>
<form method="POST" action="/admin/firmware/save">
<label for="f-version">Target version</label><input id="f-version" type="text" name="version" value="{{.View.Firmware.Version}}">
<label for="f-url">Image URL</label><input id="f-url" type="text" name="url" value="{{.View.Firmware.URL}}">
<label for="f-dir">Local serve directory</label><input id="f-dir" type="text" name="dir" value="{{.View.Firmware.Dir}}">
<button type="submit">Save firmware</button>
</form>
</section>
</main>
</div>
</body>
</html>
`))
