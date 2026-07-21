package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestDirectoryVerify(t *testing.T) {
	d := NewDirectory([]Entry{
		{Username: "alice", PasswordSHA256: hashOf("alice-pw"), Role: RoleAdmin},
		{Username: "bob", PasswordSHA256: hashOf("bob-pw"), Role: RoleManager},
		{Username: "carol", PasswordSHA256: hashOf("carol-pw"), Role: RoleReceptionist},
	})

	if role, ok := d.Verify("alice", "alice-pw"); !ok || role != RoleAdmin {
		t.Fatalf("alice: got role=%q ok=%v, want RoleAdmin, true", role, ok)
	}
	if _, ok := d.Verify("alice", "wrong"); ok {
		t.Fatal("wrong password must not verify")
	}
	if _, ok := d.Verify("alice", "bob-pw"); ok {
		t.Fatal("another account's password must not verify")
	}
	if role, ok := d.Verify("bob", "bob-pw"); !ok || role != RoleManager {
		t.Fatalf("bob: got role=%q ok=%v, want RoleManager, true", role, ok)
	}
	if role, ok := d.Verify("carol", "carol-pw"); !ok || role != RoleReceptionist {
		t.Fatalf("carol: got role=%q ok=%v, want RoleReceptionist, true", role, ok)
	}
}

func TestDirectoryUsernameIsCaseInsensitive(t *testing.T) {
	d := NewDirectory([]Entry{{Username: "Alice", PasswordSHA256: hashOf("alice-pw"), Role: RoleAdmin}})
	if _, ok := d.Verify("alice", "alice-pw"); !ok {
		t.Fatal("username lookup must be case-insensitive")
	}
	if _, ok := d.Verify("ALICE", "alice-pw"); !ok {
		t.Fatal("username lookup must be case-insensitive")
	}
}

func TestDirectoryUnknownUsernameAlwaysRejects(t *testing.T) {
	d := NewDirectory([]Entry{{Username: "alice", PasswordSHA256: hashOf("alice-pw"), Role: RoleAdmin}})
	if _, ok := d.Verify("nobody", ""); ok {
		t.Fatal("an unknown username must never verify, even against an empty password")
	}
}

func TestDirectorySkipsInvalidHash(t *testing.T) {
	d := NewDirectory([]Entry{{Username: "alice", PasswordSHA256: "not-a-real-hash", Role: RoleAdmin}})
	if _, ok := d.Verify("alice", ""); ok {
		t.Fatal("an entry with an invalid hash must never verify")
	}
}
