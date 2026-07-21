package auth

import (
	"testing"
	"time"
)

func TestSessionCreateCheckRevoke(t *testing.T) {
	s := NewSessionStore(time.Hour)

	token := s.Create(RoleAdmin, "alice", SessionFlags{})
	sess, ok := s.Check(token)
	if !ok || sess.Role != RoleAdmin {
		t.Fatalf("Check(valid token) = %+v, %v; want RoleAdmin, true", sess, ok)
	}
	if sess.Username != "alice" {
		t.Fatalf("Check(valid token).Username = %q, want alice", sess.Username)
	}

	s.Revoke(token)
	if _, ok := s.Check(token); ok {
		t.Fatal("Check after Revoke must fail")
	}

	if _, ok := s.Check("never-issued"); ok {
		t.Fatal("Check on an unknown token must fail")
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewSessionStore(1 * time.Millisecond)
	token := s.Create(RoleManager, "bob", SessionFlags{})
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Check(token); ok {
		t.Fatal("expired session must be rejected")
	}
}

func TestSessionTokensAreUnique(t *testing.T) {
	s := NewSessionStore(time.Hour)
	a, b := s.Create(RoleAdmin, "alice", SessionFlags{}), s.Create(RoleAdmin, "alice", SessionFlags{})
	if a == b {
		t.Fatal("two Create calls must not produce the same token")
	}
}
