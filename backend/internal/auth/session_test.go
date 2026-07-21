package auth

import (
	"testing"
	"time"
)

func TestSessionCreateCheckRevoke(t *testing.T) {
	s := NewSessionStore(time.Hour)

	token := s.Create(RoleAdmin)
	role, ok := s.Check(token)
	if !ok || role != RoleAdmin {
		t.Fatalf("Check(valid token) = %q, %v; want RoleAdmin, true", role, ok)
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
	token := s.Create(RoleManager)
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Check(token); ok {
		t.Fatal("expired session must be rejected")
	}
}

func TestSessionTokensAreUnique(t *testing.T) {
	s := NewSessionStore(time.Hour)
	a, b := s.Create(RoleAdmin), s.Create(RoleAdmin)
	if a == b {
		t.Fatal("two Create calls must not produce the same token")
	}
}
