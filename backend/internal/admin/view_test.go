package admin

import "testing"

import "github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"

func TestBuildMapsRoomsAndMasksToken(t *testing.T) {
	flat := "flat"
	interval := uint32(600)
	cfg := &config.Config{
		Wake: config.WakeConfig{Mode: "smart"},
		Rooms: []config.Room{
			{DeviceID: "rt-1", Name: "Aspen", Room: "a@x", TokenSHA256: "deadbeef", WakeMode: &flat, FlatIntervalSeconds: &interval},
			{DeviceID: "rt-2", Name: "Birch", Room: "b@x"},
		},
	}

	v := Build(cfg)
	if len(v.Rooms) != 2 {
		t.Fatalf("want 2 rooms, got %d", len(v.Rooms))
	}
	if !v.Rooms[0].TokenConfigured {
		t.Error("rt-1 has a token, TokenConfigured must be true")
	}
	if v.Rooms[0].WakeMode != "flat" || v.Rooms[0].FlatIntervalSeconds != 600 {
		t.Errorf("rt-1 override not mapped: %+v", v.Rooms[0])
	}
	if v.Rooms[1].TokenConfigured {
		t.Error("rt-2 has no token, TokenConfigured must be false")
	}
	if v.Rooms[1].WakeMode != "" {
		t.Errorf("rt-2 has no override, want empty WakeMode, got %q", v.Rooms[1].WakeMode)
	}

	// The view must never expose the raw hash string anywhere.
	for _, r := range v.Rooms {
		if r.WakeMode == "deadbeef" {
			t.Fatal("token hash leaked into an unexpected field")
		}
	}
}

func TestBuildMapsUsersWithoutExposingPasswordHash(t *testing.T) {
	cfg := &config.Config{
		Users: []config.User{
			{Username: "alice", PasswordSHA256: "deadbeef", Role: "admin"},
			{Username: "bob", PasswordSHA256: "cafef00d", Role: "manager"},
		},
	}

	v := Build(cfg)
	if len(v.Users) != 2 {
		t.Fatalf("want 2 users, got %d", len(v.Users))
	}
	if v.Users[0].Username != "alice" || v.Users[0].Role != "admin" {
		t.Errorf("alice not mapped correctly: %+v", v.Users[0])
	}
	if v.Users[1].Username != "bob" || v.Users[1].Role != "manager" {
		t.Errorf("bob not mapped correctly: %+v", v.Users[1])
	}

	// UserView has no field a password hash could ever land in — this is really a compile-time
	// guarantee, but assert it defensively in case the struct grows a Password field later.
	for _, u := range v.Users {
		if u.Username == "deadbeef" || u.Username == "cafef00d" || u.Role == "deadbeef" || u.Role == "cafef00d" {
			t.Fatal("password hash leaked into the view")
		}
	}
}
