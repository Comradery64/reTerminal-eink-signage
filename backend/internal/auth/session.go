// Package auth provides cookie-session authentication for the /admin and /manager web UIs. It
// mirrors internal/server/auth.go's device bearer-token pattern (precomputed hash table, constant-
// time compare) but for a username/password login that mints a server-side session token instead
// of checking a bearer header per request.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Role is an account's access level. Roles form a hierarchy, highest first: admin, then manager,
// then viewer — each one can do everything the ones below it can. An admin account can log into
// /admin, /manager, or /dashboard with the same credentials; a manager account can log into
// /manager or /dashboard; a viewer account can only log into /dashboard.
type Role string

const (
	RoleAdmin   Role = "admin"
	RoleManager Role = "manager"
	RoleViewer  Role = "viewer"
)

// roleRank orders the hierarchy — higher number outranks lower.
var roleRank = map[Role]int{
	RoleViewer:  1,
	RoleManager: 2,
	RoleAdmin:   3,
}

// Satisfies reports whether r is at least as privileged as required — e.g.
// RoleAdmin.Satisfies(RoleManager) is true, RoleViewer.Satisfies(RoleManager) is false. An
// unrecognized role satisfies nothing (rank 0), so a typo'd config.User.Role fails closed rather
// than silently granting access.
func (r Role) Satisfies(required Role) bool {
	return roleRank[r] >= roleRank[required]
}

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
//
// Sessions are keyed by the SHA-256 of the token, never the token itself, so that if the store is
// persisted (see Persist) the file holds nothing that can be replayed as a cookie.
type SessionStore struct {
	mu  sync.RWMutex
	m   map[string]sessionEntry // key(token) -> entry
	ttl time.Duration

	path  string      // "" = memory only
	onErr func(error) // told about a failed save; the store keeps working in memory either way
}

// key is how a token is stored: its SHA-256, hex.
func key(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewSessionStore builds an empty store with the given session lifetime.
func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{m: make(map[string]sessionEntry), ttl: ttl}
}

// Create mints a new random session token for role/username and stores it with a fresh expiry.
func (s *SessionStore) Create(role Role, username string, flags SessionFlags) string {
	token := newToken()
	s.mu.Lock()
	s.m[key(token)] = sessionEntry{role: role, username: username, flags: flags, expires: time.Now().Add(s.ttl)}
	s.saveLocked()
	s.mu.Unlock()
	return token
}

// Check returns (session, true) if token is present and unexpired. An expired entry is evicted on
// the read that discovers it, rather than via a background sweep — this store never sees enough
// entries for that to matter.
func (s *SessionStore) Check(token string) (Session, bool) {
	s.mu.RLock()
	e, ok := s.m[key(token)]
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
	delete(s.m, key(token))
	s.saveLocked()
	s.mu.Unlock()
}

// RevokeByUsername deletes every live session belonging to username (case-insensitive, matching
// the rest of this codebase's username handling — see config.Validate's duplicate check), e.g.
// when an admin resets that account's password: a stolen or still-open session shouldn't survive
// its own password being replaced out from under it.
func (s *SessionStore) RevokeByUsername(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if strings.EqualFold(e.username, username) {
			delete(s.m, k)
		}
	}
	s.saveLocked()
}

// persistedSession is a session as written to disk. The token itself is never stored, only its hash.
type persistedSession struct {
	Key                string    `json:"key"`
	Role               Role      `json:"role"`
	Username           string    `json:"username"`
	MustChangePassword bool      `json:"must_change_password,omitempty"`
	Pending2FA         bool      `json:"pending_2fa,omitempty"`
	Expires            time.Time `json:"expires"`
}

type persistedFile struct {
	Version  int                `json:"version"`
	Sessions []persistedSession `json:"sessions"`
}

// Persist keeps the store on disk at path so logins survive a restart: it loads any sessions
// already there (dropping expired ones) and from then on rewrites the file on every change. A
// missing file is normal (first run). A file that can't be read or parsed is reported and the store
// starts empty — everyone logs in again, which is the safe direction. onErr is told about later
// failed saves; the store keeps working in memory regardless.
func (s *SessionStore) Persist(path string, onErr func(error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path, s.onErr = path, onErr
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session store: %w", err)
	}
	var f persistedFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 {
		return fmt.Errorf("session store %s is unreadable or from another version; starting with no sessions", filepath.Base(path))
	}
	now := time.Now()
	for _, p := range f.Sessions {
		if p.Key == "" || !now.Before(p.Expires) {
			continue
		}
		s.m[p.Key] = sessionEntry{role: p.Role, username: p.Username,
			flags: SessionFlags{MustChangePassword: p.MustChangePassword, Pending2FA: p.Pending2FA}, expires: p.Expires}
	}
	return nil
}

// saveLocked rewrites the file with the live sessions. Caller holds s.mu. It writes a temp file in
// the same directory (mode 0600) and renames it over the old one, so a crash never leaves a
// half-written store.
func (s *SessionStore) saveLocked() {
	if s.path == "" {
		return
	}
	now := time.Now()
	f := persistedFile{Version: 1, Sessions: []persistedSession{}}
	for k, e := range s.m {
		if !now.Before(e.expires) {
			delete(s.m, k)
			continue
		}
		f.Sessions = append(f.Sessions, persistedSession{Key: k, Role: e.role, Username: e.username,
			MustChangePassword: e.flags.MustChangePassword, Pending2FA: e.flags.Pending2FA, Expires: e.expires})
	}
	raw, err := json.Marshal(f)
	if err == nil {
		err = writeFileAtomic(s.path, raw)
	}
	if err != nil && s.onErr != nil {
		s.onErr(fmt.Errorf("save session store: %w", err))
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sessions-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// newToken returns a 32-byte crypto/rand value, base64url-encoded for safe use as a cookie value.
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
