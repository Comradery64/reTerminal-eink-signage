package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
)

func validBase() Config {
	sum := sha256.Sum256([]byte("test-token"))
	return Config{
		Provider: "demo",
		Rooms:    []Room{{DeviceID: "rt-1", Room: "a@x", TokenSHA256: hex.EncodeToString(sum[:])}},
		Wake:     WakeConfig{Timezone: "UTC"},
	}
}

func TestValidateForcedRefreshHour(t *testing.T) {
	c := validBase()
	if err := c.Validate(); err != nil {
		t.Fatalf("nil ForcedRefreshHour (disabled) must be valid: %v", err)
	}

	ok := 3
	c.Wake.ForcedRefreshHour = &ok
	if err := c.Validate(); err != nil {
		t.Fatalf("hour 3 must be valid: %v", err)
	}

	tooHigh := 24
	c.Wake.ForcedRefreshHour = &tooHigh
	if err := c.Validate(); err == nil {
		t.Fatal("hour 24 must be rejected")
	}

	negative := -1
	c.Wake.ForcedRefreshHour = &negative
	if err := c.Validate(); err == nil {
		t.Fatal("negative hour must be rejected")
	}
}

func TestNextWakeSecondsLandsOnOffsetGrid(t *testing.T) {
	c := Config{Wake: WakeConfig{
		Timezone:             "America/Los_Angeles",
		BusinessStartHour:    10,
		BusinessEndHour:      18,
		BusinessHoursSeconds: 900,
		OffHoursSeconds:      3600,
	}}
	loc := c.Location()

	cases := []struct {
		name string
		now  time.Time
		want time.Time // expected wake instant
	}{
		{
			name: "business hours, mid-slot",
			now:  time.Date(2026, 7, 20, 15, 21, 22, 0, loc), // Monday 3:21:22pm
			want: time.Date(2026, 7, 20, 15, 35, 0, 0, loc),  // next :05/:20/:35/:50 mark
		},
		{
			name: "business hours, exactly on an unoffset boundary",
			now:  time.Date(2026, 7, 20, 15, 30, 0, 0, loc),
			want: time.Date(2026, 7, 20, 15, 35, 0, 0, loc),
		},
		{
			name: "off hours, mid-hour",
			now:  time.Date(2026, 7, 20, 21, 10, 0, 0, loc),
			want: time.Date(2026, 7, 20, 22, 5, 0, 0, loc), // next hour mark + 5min offset
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.now.Add(time.Duration(c.flatWakeSeconds(Room{}, tc.now)) * time.Second)
			if !got.Equal(tc.want) {
				t.Fatalf("now=%s: got wake at %s, want %s", tc.now, got, tc.want)
			}
		})
	}
}

func smartConfig() Config {
	return Config{Wake: WakeConfig{
		Mode:                 "smart",
		Timezone:             "America/Los_Angeles",
		BusinessStartHour:    10,
		BusinessEndHour:      18,
		BusinessHoursSeconds: 15 * 60,
		OffHoursSeconds:      3600,
	}}
}

func mkEvent(startOffset, durMin int, base time.Time) calendar.Event {
	s := base.Add(time.Duration(startOffset) * time.Minute)
	return calendar.Event{Subject: "m", Start: s, End: s.Add(time.Duration(durMin) * time.Minute)}
}

func TestWakeModeResolution(t *testing.T) {
	c := Config{Wake: WakeConfig{}}
	if got := c.wakeModeFor(Room{}); got != "flat" {
		t.Fatalf("no fleet mode set: want flat, got %q", got)
	}

	c.Wake.Mode = "smart"
	if got := c.wakeModeFor(Room{}); got != "smart" {
		t.Fatalf("fleet mode smart: want smart, got %q", got)
	}

	flat := "flat"
	if got := c.wakeModeFor(Room{WakeMode: &flat}); got != "flat" {
		t.Fatalf("room override should win over fleet default: got %q", got)
	}
}

func TestSmartWakeSecondsBusinessHoursCap(t *testing.T) {
	c := smartConfig()
	loc := c.Location()
	now := time.Date(2026, 7, 20, 11, 0, 0, 0, loc) // Monday 11am, business hours

	next := mkEvent(180, 30, now) // 3 hours away — far beyond the 15min cap
	got := c.NextWakeDuration(Room{}, nil, &next, now)
	if got != c.Wake.BusinessHoursSeconds {
		t.Fatalf("want capped at %d, got %d", c.Wake.BusinessHoursSeconds, got)
	}
}

func TestSmartWakeSecondsBusinessHoursUncappedWhenSooner(t *testing.T) {
	c := smartConfig()
	loc := c.Location()
	now := time.Date(2026, 7, 20, 11, 0, 0, 0, loc)

	next := mkEvent(5, 30, now) // starts in 5 min — well under the 15min cap
	got := c.NextWakeDuration(Room{}, nil, &next, now)
	want := uint32(5*60 + int(calendar.TransitionBuffer.Seconds()))
	if got != want {
		t.Fatalf("want %d, got %d", want, got)
	}
}

func TestSmartWakeSecondsOffHoursUncapped(t *testing.T) {
	c := smartConfig()
	loc := c.Location()
	now := time.Date(2026, 7, 20, 21, 0, 0, 0, loc) // 9pm, off hours

	next := mkEvent(180, 30, now) // 3 hours away — no cap should apply off-hours
	got := c.NextWakeDuration(Room{}, nil, &next, now)
	want := uint32((180-10)*60 + int(calendar.TransitionBuffer.Seconds())) // minus starting-soon lead
	if got != want {
		t.Fatalf("want %d (uncapped), got %d", want, got)
	}
}

func TestSmartWakeSecondsOffHoursNothingScheduledSleepsToBusinessStart(t *testing.T) {
	c := smartConfig()
	loc := c.Location()
	now := time.Date(2026, 7, 20, 21, 0, 0, 0, loc) // 9pm Monday, nothing on the calendar

	got := c.NextWakeDuration(Room{}, nil, nil, now)
	want := time.Date(2026, 7, 21, 10, 0, 0, 0, loc).Sub(now) // next day, 10am business start
	if got != uint32(want.Seconds()) {
		t.Fatalf("want %d (until 10am next day), got %d", uint32(want.Seconds()), got)
	}
}

func TestSmartWakeSecondsBusinessHoursNothingScheduled(t *testing.T) {
	c := smartConfig()
	loc := c.Location()
	now := time.Date(2026, 7, 20, 11, 0, 0, 0, loc)

	got := c.NextWakeDuration(Room{}, nil, nil, now)
	if got != c.Wake.BusinessHoursSeconds {
		t.Fatalf("want periodic safety-net %d, got %d", c.Wake.BusinessHoursSeconds, got)
	}
}

func TestWithRoomAddsAndEdits(t *testing.T) {
	c := validBase()

	sum := sha256.Sum256([]byte("room-2-token"))
	added, err := c.WithRoom(Room{DeviceID: "rt-2", Room: "b@x", TokenSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(added.Rooms) != 2 {
		t.Fatalf("want 2 rooms after add, got %d", len(added.Rooms))
	}
	if len(c.Rooms) != 1 {
		t.Fatalf("original config must be unmodified, got %d rooms", len(c.Rooms))
	}

	renamed, err := added.WithRoom(Room{DeviceID: "rt-2", Room: "b@x", Name: "Renamed", TokenSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(renamed.Rooms) != 2 {
		t.Fatalf("edit must not change room count, got %d", len(renamed.Rooms))
	}
	got, _ := renamed.RoomByDeviceID("rt-2")
	if got.Name != "Renamed" {
		t.Fatalf("edit did not apply: got name %q", got.Name)
	}
}

func TestWithRoomRejectsInvalidResult(t *testing.T) {
	c := validBase()
	if _, err := c.WithRoom(Room{DeviceID: "rt-2", Room: "b@x", TokenSHA256: "not-hex"}); err == nil {
		t.Fatal("bad token_sha256 must be rejected")
	}
	if _, err := c.WithRoom(Room{DeviceID: "rt-1", Room: "a@x", TokenSHA256: c.Rooms[0].TokenSHA256}); err != nil {
		t.Fatalf("editing an existing device_id in place must not be treated as a duplicate: %v", err)
	}
}

func TestWithoutRoomRemovesAndRejectsLastRoom(t *testing.T) {
	c := validBase()
	sum := sha256.Sum256([]byte("room-2-token"))
	c2, err := c.WithRoom(Room{DeviceID: "rt-2", Room: "b@x", TokenSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	c1, err := c2.WithoutRoom("rt-2")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(c1.Rooms) != 1 {
		t.Fatalf("want 1 room after removal, got %d", len(c1.Rooms))
	}

	if _, err := c1.WithoutRoom("rt-1"); err == nil {
		t.Fatal("removing the last room must be rejected")
	}
}

func TestWithRoomWakeOverride(t *testing.T) {
	c := validBase()

	smart := "smart"
	updated, err := c.WithRoomWakeOverride("rt-1", &smart, nil)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	got, _ := updated.RoomByDeviceID("rt-1")
	if got.WakeMode == nil || *got.WakeMode != "smart" {
		t.Fatalf("wake mode override did not apply: %+v", got.WakeMode)
	}
	if c.Rooms[0].WakeMode != nil {
		t.Fatal("original config must be unmodified")
	}

	if _, err := c.WithRoomWakeOverride("does-not-exist", &smart, nil); err == nil {
		t.Fatal("unknown device_id must be rejected")
	}

	bogus := "bogus-mode"
	if _, err := c.WithRoomWakeOverride("rt-1", &bogus, nil); err == nil {
		t.Fatal("invalid wake_mode must be rejected")
	}
}

func TestUsersEmptyIsValid(t *testing.T) {
	c := validBase() // no Users at all
	if err := c.Validate(); err != nil {
		t.Fatalf("no users configured (login disabled) must be valid: %v", err)
	}
}

func TestWithUserAddEditRequiresAdminAndSessionSecret(t *testing.T) {
	c := validBase()

	// Adding a non-admin user with no admin present yet must be rejected.
	if _, err := c.WithUser(User{Username: "bob", PasswordSHA256: hashOf64("bob-pw"), Role: "manager"}); err == nil {
		t.Fatal("first user must include at least one admin")
	}

	c.Auth.SessionSecret = "01234567890123456789012345678901"
	withAdmin, err := c.WithUser(User{Username: "alice", PasswordSHA256: hashOf64("alice-pw"), Role: "admin"})
	if err != nil {
		t.Fatalf("add admin: %v", err)
	}
	if len(withAdmin.Users) != 1 {
		t.Fatalf("want 1 user, got %d", len(withAdmin.Users))
	}
	if len(c.Users) != 0 {
		t.Fatal("original config must be unmodified")
	}

	withBoth, err := withAdmin.WithUser(User{Username: "bob", PasswordSHA256: hashOf64("bob-pw"), Role: "manager"})
	if err != nil {
		t.Fatalf("add manager: %v", err)
	}
	if len(withBoth.Users) != 2 {
		t.Fatalf("want 2 users, got %d", len(withBoth.Users))
	}

	// Editing an existing username (case-insensitive match) replaces rather than duplicates.
	renamed, err := withBoth.WithUser(User{Username: "Bob", PasswordSHA256: hashOf64("new-pw"), Role: "viewer"})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(renamed.Users) != 2 {
		t.Fatalf("edit must not change user count, got %d", len(renamed.Users))
	}
	got, ok := renamed.UserByUsername("bob")
	if !ok || got.Role != "viewer" {
		t.Fatalf("edit did not apply: %+v", got)
	}
}

func TestWithUserRejectsInvalidRole(t *testing.T) {
	c := validBase()
	c.Auth.SessionSecret = "01234567890123456789012345678901"
	if _, err := c.WithUser(User{Username: "alice", PasswordSHA256: hashOf64("alice-pw"), Role: "superadmin"}); err == nil {
		t.Fatal("invalid role must be rejected")
	}
}

func TestWithoutUserRejectsLeavingUsersWithNoAdmin(t *testing.T) {
	c := validBase()
	c.Auth.SessionSecret = "01234567890123456789012345678901"

	// Removing the sole remaining account (down to zero users entirely) is allowed — that's just
	// fully disabling login, the same valid state as never having configured any users.
	withAdmin, err := c.WithUser(User{Username: "alice", PasswordSHA256: hashOf64("alice-pw"), Role: "admin"})
	if err != nil {
		t.Fatalf("add admin: %v", err)
	}
	if _, err := withAdmin.WithoutUser("alice"); err != nil {
		t.Fatalf("removing down to zero users must succeed (disables login): %v", err)
	}

	// But removing the only admin while a non-admin account remains must be rejected — bob would
	// be left able to log in with nobody able to administer the fleet.
	withBoth, err := withAdmin.WithUser(User{Username: "bob", PasswordSHA256: hashOf64("bob-pw"), Role: "manager"})
	if err != nil {
		t.Fatalf("add manager: %v", err)
	}
	if _, err := withBoth.WithoutUser("alice"); err == nil {
		t.Fatal("removing the last admin while other users remain must be rejected")
	}

	withTwoAdmins, err := withBoth.WithUser(User{Username: "carol", PasswordSHA256: hashOf64("carol-pw"), Role: "admin"})
	if err != nil {
		t.Fatalf("add second admin: %v", err)
	}
	afterRemove, err := withTwoAdmins.WithoutUser("alice")
	if err != nil {
		t.Fatalf("removing one of two admins must succeed: %v", err)
	}
	if len(afterRemove.Users) != 2 {
		t.Fatalf("want 2 users remaining, got %d", len(afterRemove.Users))
	}
}

func hashOf64(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestWithWakeAlertsFirmware(t *testing.T) {
	c := validBase()

	updated, err := c.WithWake(WakeConfig{Timezone: "America/Los_Angeles", Mode: "smart"})
	if err != nil {
		t.Fatalf("WithWake: %v", err)
	}
	if updated.Wake.Mode != "smart" || c.Wake.Mode == "smart" {
		t.Fatal("WithWake must not mutate the receiver")
	}

	if _, err := c.WithWake(WakeConfig{Timezone: "not-a-real-zone"}); err == nil {
		t.Fatal("invalid timezone must be rejected")
	}

	updated, err = c.WithAlerts(AlertConfig{LowBatteryPct: 30, ClearPct: 50, MinRenotify: time.Hour, StaleAfter: time.Hour})
	if err != nil {
		t.Fatalf("WithAlerts: %v", err)
	}
	if updated.Alerts.LowBatteryPct != 30 {
		t.Fatal("WithAlerts did not apply")
	}

	updated, err = c.WithFirmware(FirmwareConfig{Version: "2.0.0", URL: "https://x/fw.bin"})
	if err != nil {
		t.Fatalf("WithFirmware: %v", err)
	}
	if updated.Firmware.Version != "2.0.0" {
		t.Fatal("WithFirmware did not apply")
	}
}

// TestApplyDefaultsTelemetryAndConfigPersistence pins today's-behavior-unchanged defaults: an
// existing config.yaml that never mentions telemetry: or config_persistence: must resolve to
// exactly these values (memory telemetry, auto persistence, the configmap coordinates that used to
// be hardcoded constants in internal/server/configwrite.go).
func TestApplyDefaultsTelemetryAndConfigPersistence(t *testing.T) {
	c := validBase()
	c.applyDefaults()

	if c.Telemetry.Backend != "memory" {
		t.Errorf("telemetry.backend default = %q, want memory", c.Telemetry.Backend)
	}
	if c.Telemetry.SQLite.Path != "/var/lib/meeting-displays/telemetry.db" {
		t.Errorf("telemetry.sqlite.path default = %q", c.Telemetry.SQLite.Path)
	}
	if c.Telemetry.SQLite.RetentionDays != 90 {
		t.Errorf("telemetry.sqlite.retention_days default = %d, want 90", c.Telemetry.SQLite.RetentionDays)
	}
	if c.Telemetry.SQLite.PruneInterval != 6*time.Hour {
		t.Errorf("telemetry.sqlite.prune_interval default = %v, want 6h", c.Telemetry.SQLite.PruneInterval)
	}
	if c.Telemetry.SQLite.QueueSize != 256 {
		t.Errorf("telemetry.sqlite.queue_size default = %d, want 256", c.Telemetry.SQLite.QueueSize)
	}
	if c.ConfigPersistence.Mode != "auto" {
		t.Errorf("config_persistence.mode default = %q, want auto", c.ConfigPersistence.Mode)
	}
	cm := c.ConfigPersistence.ConfigMap
	if cm.Namespace != "meeting-displays" || cm.Name != "broker-config" || cm.Key != "config.yaml" {
		t.Errorf("config_persistence.configmap defaults = %+v, want today's hardcoded values", cm)
	}
}

func TestValidateRejectsUnknownTelemetryBackendAndPersistenceMode(t *testing.T) {
	c := validBase()
	c.Telemetry.Backend = "bogus"
	if err := c.Validate(); err == nil {
		t.Fatal("unknown telemetry.backend must be rejected")
	}

	c = validBase()
	c.ConfigPersistence.Mode = "bogus"
	if err := c.Validate(); err == nil {
		t.Fatal("unknown config_persistence.mode must be rejected")
	}
}

// TestMarshalForPersistRestoresEnvRefs guards the live secret leak this change fixes: Load expands
// ${VAR} for in-memory use, but writing that expanded value back out (to a ConfigMap, or now to a
// plain file) would copy a secret out of wherever it was actually provisioned. MarshalForPersist
// must restore the ${VAR} token instead.
func TestMarshalForPersistRestoresEnvRefs(t *testing.T) {
	const secretEnvVar = "TEST_SESSION_SECRET_FOR_MARSHAL_ROUNDTRIP"
	secret := strings.Repeat("s3cr3t-", 6) // well over the 8-byte floor, never printed
	t.Setenv(secretEnvVar, secret)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlContent := `
provider: demo
rooms:
  - device_id: rt-1
    room: a@x
    token_sha256: ` + hashOf64("t") + `
wake:
  timezone: UTC
auth:
  session_secret: "${` + secretEnvVar + `}"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Auth.SessionSecret != secret {
		t.Fatal("the expanded secret must be usable in memory")
	}

	// Apply a normal admin-style edit — MarshalForPersist must still restore the token afterward,
	// since clone() carries envRefs forward.
	updated, err := c.WithRoom(Room{DeviceID: "rt-1", Room: "a@x", TokenSHA256: c.Rooms[0].TokenSHA256, Name: "renamed"})
	if err != nil {
		t.Fatalf("WithRoom: %v", err)
	}

	out, err := updated.MarshalForPersist()
	if err != nil {
		t.Fatalf("MarshalForPersist: %v", err)
	}
	outStr := string(out)
	if strings.Contains(outStr, secret) {
		t.Fatal("MarshalForPersist must never write the expanded secret out")
	}
	if !strings.Contains(outStr, "${"+secretEnvVar+"}") {
		t.Fatalf("MarshalForPersist must restore the ${VAR} token; got:\n%s", outStr)
	}
}

func TestPerRoomWakeModeOverride(t *testing.T) {
	c := smartConfig() // fleet default is smart
	loc := c.Location()
	now := time.Date(2026, 7, 20, 11, 0, 0, 0, loc)

	flat := "flat"
	room := Room{WakeMode: &flat}
	next := mkEvent(180, 30, now)
	got := c.NextWakeDuration(room, nil, &next, now)
	// Flat-mode result should match flatWakeSeconds exactly (grid-aligned business interval),
	// not the smart-mode transition/cap logic.
	want := c.flatWakeSeconds(room, now)
	if got != want {
		t.Fatalf("room override to flat: want %d (flat grid), got %d", want, got)
	}
}

// A room may legitimately have several displays (a door plaque plus an interior panel), so
// duplicate room addresses must stay valid — but only when Label tells them apart, because
// otherwise /dashboard and /status would show indistinguishable cards for two physical devices.
func TestValidateMultipleDisplaysPerRoom(t *testing.T) {
	sum := sha256.Sum256([]byte("test-token"))
	hash := hex.EncodeToString(sum[:])
	room := func(id, addr, label string) Room {
		return Room{DeviceID: id, Name: "Aspen", Room: addr, Label: label, TokenSHA256: hash}
	}

	tests := []struct {
		name    string
		rooms   []Room
		wantErr string
	}{
		{
			name:  "single display needs no label",
			rooms: []Room{room("rt-1", "a@x", "")},
		},
		{
			name:  "distinct rooms need no labels",
			rooms: []Room{room("rt-1", "a@x", ""), room("rt-2", "b@x", "")},
		},
		{
			name:  "two displays in one room, both labelled",
			rooms: []Room{room("rt-1", "a@x", "Door"), room("rt-2", "a@x", "Interior wall")},
		},
		{
			name:    "two displays in one room, one unlabelled",
			rooms:   []Room{room("rt-1", "a@x", "Door"), room("rt-2", "a@x", "")},
			wantErr: "needs a label",
		},
		{
			name:    "two displays in one room, neither labelled",
			rooms:   []Room{room("rt-1", "a@x", ""), room("rt-2", "a@x", "")},
			wantErr: "needs a label",
		},
		{
			name:    "duplicate labels within a room",
			rooms:   []Room{room("rt-1", "a@x", "Door"), room("rt-2", "a@x", "Door")},
			wantErr: "must be distinct",
		},
		{
			name:    "labels differing only by case still collide",
			rooms:   []Room{room("rt-1", "a@x", "Door"), room("rt-2", "a@x", "door")},
			wantErr: "must be distinct",
		},
		{
			name:    "room addresses differing only by case are the same room",
			rooms:   []Room{room("rt-1", "a@x", "Door"), room("rt-2", "A@X", "")},
			wantErr: "needs a label",
		},
		{
			name:  "the same label in different rooms is fine",
			rooms: []Room{room("rt-1", "a@x", "Door"), room("rt-2", "a@x", "Wall"), room("rt-3", "b@x", "Door")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validBase()
			c.Rooms = tc.rooms
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want valid, got error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want an error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
