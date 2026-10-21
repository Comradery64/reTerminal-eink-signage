package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func storeAt(t *testing.T, path string, ttl time.Duration) (*SessionStore, error) {
	t.Helper()
	s := NewSessionStore(ttl)
	return s, s.Persist(path, func(error) {})
}

// A login made before a restart is still valid after it, with the role, account and flags intact.
func TestSessionsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a, err := storeAt(t, path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tok := a.Create(RoleManager, "alex", SessionFlags{MustChangePassword: true, Pending2FA: true})
	other := a.Create(RoleViewer, "desk", SessionFlags{})

	b, err := storeAt(t, path, time.Hour) // "restart"
	if err != nil {
		t.Fatal(err)
	}
	got, ok := b.Check(tok)
	if !ok || got.Role != RoleManager || got.Username != "alex" || !got.MustChangePassword || !got.Pending2FA {
		t.Fatalf("restored session = %+v ok=%v", got, ok)
	}
	if _, ok := b.Check(other); !ok {
		t.Error("a second session was lost")
	}
	if _, ok := b.Check("not-a-token"); ok {
		t.Error("an unknown token must not be valid")
	}
}

// The file holds a hash of each token, never the token, and is private to the owner.
func TestPersistedStoreHoldsNoUsableToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a, _ := storeAt(t, path, time.Hour)
	tok := a.Create(RoleAdmin, "root", SessionFlags{})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tok) {
		t.Fatal("the session token is written to disk in the clear")
	}
	if !strings.Contains(string(raw), key(tok)) {
		t.Error("expected the token's hash in the file")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %v, want 0600", fi.Mode().Perm())
	}
}

// Logging out survives a restart: a revoked session must not come back. (Checked on its own so a
// later save from some other change can't hide a missing one.)
func TestLogoutSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a, _ := storeAt(t, path, time.Hour)
	out := a.Create(RoleManager, "alex", SessionFlags{})
	keep := a.Create(RoleViewer, "desk", SessionFlags{})
	a.Revoke(out)
	b, _ := storeAt(t, path, time.Hour)
	if _, ok := b.Check(out); ok {
		t.Error("a logged-out session came back after a restart")
	}
	if _, ok := b.Check(keep); !ok {
		t.Error("an unrelated session was dropped")
	}
}

// Revoking an account's sessions (a password or 2FA reset) survives a restart too.
func TestRevokeByUsernameSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a, _ := storeAt(t, path, time.Hour)
	gone := a.Create(RoleViewer, "ALEX", SessionFlags{})
	keep := a.Create(RoleViewer, "desk", SessionFlags{})
	a.RevokeByUsername("alex")
	b, _ := storeAt(t, path, time.Hour)
	if _, ok := b.Check(gone); ok {
		t.Error("a session revoked by username came back after a restart")
	}
	if _, ok := b.Check(keep); !ok {
		t.Error("an unrelated session was dropped")
	}
}

// Sessions that have expired are not restored.
func TestExpiredSessionsAreNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a, _ := storeAt(t, path, 20*time.Millisecond)
	tok := a.Create(RoleViewer, "desk", SessionFlags{})
	time.Sleep(40 * time.Millisecond)
	b, _ := storeAt(t, path, time.Hour)
	if _, ok := b.Check(tok); ok {
		t.Fatal("an expired session was restored")
	}
}

// An unreadable file means everyone signs in again — reported, never fatal, and the store works.
func TestCorruptStoreStartsEmptyAndKeepsWorking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := storeAt(t, path, time.Hour)
	if err == nil {
		t.Fatal("a corrupt store must be reported")
	}
	tok := s.Create(RoleViewer, "desk", SessionFlags{})
	if _, ok := s.Check(tok); !ok {
		t.Fatal("the store must still work after a bad file")
	}
	b, err := storeAt(t, path, time.Hour)
	if err != nil {
		t.Fatalf("the next save should have replaced the bad file: %v", err)
	}
	if _, ok := b.Check(tok); !ok {
		t.Error("the new session was not saved over the corrupt file")
	}
}

// A path that can't be written is reported and the store carries on in memory.
func TestUnwritableStoreKeepsWorking(t *testing.T) {
	var saveErr error
	s := NewSessionStore(time.Hour)
	if err := s.Persist(filepath.Join(t.TempDir(), "missing-dir", "sessions.json"), func(e error) { saveErr = e }); err != nil {
		t.Fatalf("a missing file is normal on first run: %v", err)
	}
	tok := s.Create(RoleViewer, "desk", SessionFlags{})
	if saveErr == nil || errors.Is(saveErr, os.ErrNotExist) == false {
		t.Errorf("a failed save must be reported, got %v", saveErr)
	}
	if _, ok := s.Check(tok); !ok {
		t.Error("the session must still be valid in memory")
	}
}

// Without Persist nothing is ever written.
func TestMemoryOnlyStoreWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(time.Hour)
	s.Create(RoleViewer, "desk", SessionFlags{})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("unexpected files: %v", entries)
	}
}
