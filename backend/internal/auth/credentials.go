package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// Entry is one named account as loaded from config.User — everything Directory needs to verify a
// login and know which role it grants.
type Entry struct {
	Username           string
	PasswordSHA256     string // hex, as stored in config.User
	Role               Role
	MustChangePassword bool
	TOTPEnabled        bool // true if config.User.TOTPSecret is non-empty; the secret itself never enters Directory
}

// Directory holds every configured login account, keyed by lowercased username so lookups are
// case-insensitive. Mirrors internal/server/auth.go's deviceAuth shape: a precomputed hash table
// checked in constant time.
type Directory struct {
	byUsername map[string]entryHash
}

type entryHash struct {
	hash               []byte // 32-byte sha256
	role               Role
	mustChangePassword bool
	totpEnabled        bool
}

// NewDirectory builds a Directory from config.User entries. An entry with an invalid (non-64-hex)
// password hash is skipped — config.Validate() should already reject that before it reaches here,
// so this is a defensive fallback, not the primary check.
func NewDirectory(entries []Entry) *Directory {
	m := make(map[string]entryHash, len(entries))
	for _, e := range entries {
		if b := decodeHash(e.PasswordSHA256); b != nil {
			m[strings.ToLower(e.Username)] = entryHash{hash: b, role: e.Role, mustChangePassword: e.MustChangePassword, totpEnabled: e.TOTPEnabled}
		}
	}
	return &Directory{byUsername: m}
}

func decodeHash(hexHash string) []byte {
	b, err := hex.DecodeString(hexHash)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

// VerifyResult is what a successful Verify reveals about the account: its role, whether it's
// still carrying an admin-chosen password the account holder hasn't replaced yet, and whether a
// second TOTP factor must be checked before a full session is granted.
type VerifyResult struct {
	Role               Role
	MustChangePassword bool
	TOTPEnabled        bool
}

// Verify checks username+password against the directory and, if they match, returns the
// account's role. Both an unknown username and a wrong password return ok=false with no other
// distinguishing detail returned to the caller (mirrors deviceAuth.verify's same tradeoff — an
// internal tool, not hardened against a username-enumeration timing side-channel).
func (d *Directory) Verify(username, password string) (VerifyResult, bool) {
	e, ok := d.byUsername[strings.ToLower(username)]
	if !ok {
		return VerifyResult{}, false
	}
	got := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(got[:], e.hash) != 1 {
		return VerifyResult{}, false
	}
	return VerifyResult{Role: e.role, MustChangePassword: e.mustChangePassword, TOTPEnabled: e.totpEnabled}, true
}
