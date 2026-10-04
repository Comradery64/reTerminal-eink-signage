package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"net/http"
	"sync"
	"time"
)

// passwordResetTTL bounds how long a minted temporary password stays viewable. Short: unlike the
// provisioning nonce (which gates a device flash in progress), this gates a plaintext credential
// sitting in memory purely so the admin who triggered it can read it once.
const passwordResetTTL = 5 * time.Minute

// passwordReset is one freshly reset account's plaintext temporary password, viewable exactly
// once. It is never written to disk or logged — same posture as provisionArtifact's device token.
type passwordReset struct {
	username string
	password string
	expires  time.Time
}

// passwordResetStore mirrors provisionStore's nonce/TTL/sweep-on-write shape (see provision.go) —
// same problem (a secret minted server-side, revealed once behind an unguessable URL), so the same
// solution, not a second one invented from scratch.
type passwordResetStore struct {
	mu sync.Mutex
	m  map[string]*passwordReset
}

func newPasswordResetStore() *passwordResetStore {
	return &passwordResetStore{m: map[string]*passwordReset{}}
}

func (p *passwordResetStore) put(nonce string, r *passwordReset) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, v := range p.m {
		if now.After(v.expires) {
			delete(p.m, k)
		}
	}
	p.m[nonce] = r
}

// take returns the reset and deletes it — a view is a consume, so reloading the page a second
// time (or anyone who later finds the nonce in browser history) finds nothing.
func (p *passwordResetStore) take(nonce string) (*passwordReset, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.m[nonce]
	if !ok {
		return nil, false
	}
	delete(p.m, nonce)
	if time.Now().After(r.expires) {
		return nil, false
	}
	return r, true
}

// handleAdminResetPassword mints a random temporary password for an existing user, in place of an
// admin inventing and typing one themselves — the gap that made the old "set a password" field
// read as "create an account" rather than "reset this one". Sets MustChangePassword (the account
// can't do anything else until it's replaced) and revokes every live session for that username, so
// a session open before the reset doesn't survive it.
func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.PostForm.Get("username")
	cfg := s.cfg.Load()
	existing, ok := cfg.UserByUsername(username)
	if !ok {
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper("no such user "+username)+"#access", http.StatusSeeOther)
		return
	}

	// 24 bytes of entropy, base64url — same scheme as the device-provisioning token (provision.go),
	// long enough that guessing it before the account holder changes it isn't a realistic attack.
	raw := make([]byte, 24)
	nonceRaw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		s.log.Error("password reset generation failed", "username", username, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := rand.Read(nonceRaw); err != nil {
		s.log.Error("password reset nonce generation failed", "username", username, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	nonce := base64.RawURLEncoding.EncodeToString(nonceRaw)
	sum := sha256.Sum256([]byte(password))

	existing.PasswordSHA256 = hex.EncodeToString(sum[:])
	existing.MustChangePassword = true
	newCfg, err := cfg.WithUser(existing)
	if err != nil {
		s.log.Error("admin password reset rejected", "username", username, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error())+"#access", http.StatusSeeOther)
		return
	}
	if err := s.applyConfig(r.Context(), newCfg, true); err != nil { // password hash changed — rebuild the login directory immediately
		s.log.Error("admin password reset failed to persist", "username", username, "err", err)
		http.Redirect(w, r, "/admin?error="+template.URLQueryEscaper(err.Error())+"#access", http.StatusSeeOther)
		return
	}
	s.sessions.RevokeByUsername(username)

	s.resets.put(nonce, &passwordReset{username: username, password: password, expires: time.Now().Add(passwordResetTTL)})
	s.log.Info("admin reset a user's password", "username", username)
	http.Redirect(w, r, "/admin?reset="+nonce+"#access", http.StatusSeeOther)
}
