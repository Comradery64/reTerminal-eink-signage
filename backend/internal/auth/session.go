// Package auth provides cookie-session authentication for the /admin and /manager web UIs. It
// mirrors internal/server/auth.go's device bearer-token pattern (precomputed hash table, constant-
// time compare) but for a username/password login that mints a server-side session token instead
// of checking a bearer header per request.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// Role identifies which login gate a session was created by. Roles are independent — holding one
// never grants another. RoleManager is the full building-manager tier (status + wake-mode
// control); RoleViewer is a read-only subset (status only, no controls) for front-desk staff who
// don't need or shouldn't have write access.
type Role string

const (
	RoleAdmin   Role = "admin"
	RoleManager Role = "manager"
	RoleViewer  Role = "viewer"
)

type sessionEntry struct {
	role     Role
	username string
	flags    SessionFlags
	expires  time.Time
}

// SessionFlags are the "this session can't do anything else yet" states a login can be created
// in. Both are resolved the same way: requireRole redirects every other request to the one path
// that can clear the flag, then the session is reissued with it false.
type SessionFlags struct {
	// MustChangePassword: an admin just set/reset this account's password (see
	// handleAdminSaveUser) — the account can't be used for anything until the holder replaces it.
	MustChangePassword bool
	// Pending2FA: the account has TOTP enrolled (see handleTOTPSetupSubmit) and this login has
	// passed the password check but not yet the second factor.
	Pending2FA bool
}

// Session is what Check reveals about a live token: which role it was created for, which account
// it belongs to (so a forced password-change or 2FA handler can update the right User without
// trusting a client-supplied username), and which flags are still gating it.
type Session struct {
	Role     Role
	Username string
	SessionFlags
}

// SessionStore is an in-memory token->role map with expiry. Sized for a handful of concurrent
// IT/manager sessions, not a fleet-scale structure — a plain sync.RWMutex is plenty. In-memory
// (rather than a signed stateless cookie) is deliberate: this is a single-replica broker, so
// there's no cross-instance session-sharing problem to solve, and a server-side map gets instant
// logout/revocation for free.
type SessionStore struct {
	mu  sync.RWMutex
	m   map[string]sessionEntry
	ttl time.Duration
}

// NewSessionStore builds an empty store with the given session lifetime.
func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{m: make(map[string]sessionEntry), ttl: ttl}
}

// Create mints a new random session token for role/username and stores it with a fresh expiry.
func (s *SessionStore) Create(role Role, username string, flags SessionFlags) string {
	token := newToken()
	s.mu.Lock()
	s.m[token] = sessionEntry{role: role, username: username, flags: flags, expires: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return token
}

// Check returns (session, true) if token is present and unexpired. An expired entry is evicted on
// the read that discovers it, rather than via a background sweep — this store never sees enough
// entries for that to matter.
func (s *SessionStore) Check(token string) (Session, bool) {
	s.mu.RLock()
	e, ok := s.m[token]
	s.mu.RUnlock()
	if !ok {
		return Session{}, false
	}
	if time.Now().After(e.expires) {
		s.Revoke(token)
		return Session{}, false
	}
	return Session{Role: e.role, Username: e.username, SessionFlags: e.flags}, true
}

// Revoke deletes token, e.g. on logout.
func (s *SessionStore) Revoke(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// newToken returns a 32-byte crypto/rand value, base64url-encoded for safe use as a cookie value.
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
