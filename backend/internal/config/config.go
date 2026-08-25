// Package config loads and validates broker configuration from YAML + environment.
package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
)

type Config struct {
	Listen            string                  `yaml:"listen"`
	PollInterval      time.Duration           `yaml:"poll_interval"`
	Wake              WakeConfig              `yaml:"wake"`
	Render            RenderConfig            `yaml:"render"`
	Provider          string                  `yaml:"provider"`
	Google            GoogleConfig            `yaml:"google"`
	Alerts            AlertConfig             `yaml:"alerts"`
	Firmware          FirmwareConfig          `yaml:"firmware"`
	Fleet             FleetConfig             `yaml:"fleet"`
	Auth              AuthConfig              `yaml:"auth"`
	Telemetry         TelemetryConfig         `yaml:"telemetry"`
	ConfigPersistence ConfigPersistenceConfig `yaml:"config_persistence"`
	Rooms             []Room                  `yaml:"rooms"`
	Users             []User                  `yaml:"users"`

	// envRefs records every ${VAR} -> expanded-value substitution Load performed, so
	// MarshalForPersist can undo them before a config ever gets written back out. Unexported: never
	// round-trips through YAML, and clone() carries it forward via Config's plain struct copy since
	// it's never mutated after Load (only ever read).
	envRefs map[string]string
}

// TelemetryConfig selects where telemetry history lives, beyond the in-memory "latest report per
// device" view that /metrics, /status, and /dashboard always have. Omitting this block entirely
// (an existing config.yaml) resolves to "memory" — today's behavior exactly.
type TelemetryConfig struct {
	Backend string                `yaml:"backend"` // "memory" (default) | "sqlite" | "prometheus"
	SQLite  TelemetrySQLiteConfig `yaml:"sqlite"`
}

// TelemetrySQLiteConfig configures the optional local time-series backend (see
// internal/telemetry's Sink) — pure-Go modernc.org/sqlite, no cgo, so the broker stays a single
// static binary.
type TelemetrySQLiteConfig struct {
	Path string `yaml:"path"`
	// RetentionDays deliberately breaks this repo's usual duration-string convention (e.g.
	// AlertConfig.MinRenotify's "24h") — spelling "90 days" as "2160h" is a readability trap for an
	// operator tuning retention, who reasons in days, not hours. 0 means keep forever (unbounded
	// growth on disk; documented as unsupported-but-allowed, not recommended).
	RetentionDays int           `yaml:"retention_days"`
	PruneInterval time.Duration `yaml:"prune_interval"`
	QueueSize     int           `yaml:"queue_size"` // in-flight writes buffered off the device POST path
}

// ConfigPersistenceConfig selects where a live /admin or /manager edit goes to become durable.
// Omitting this block entirely (an existing config.yaml) resolves to "auto", which reproduces
// today's behavior exactly: the in-cluster ConfigMap when running in k3s, nowhere otherwise.
type ConfigPersistenceConfig struct {
	Mode      string                 `yaml:"mode"` // "auto"(default)|"file"|"configmap"|"none"
	File      PersistFileConfig      `yaml:"file"`
	ConfigMap PersistConfigMapConfig `yaml:"configmap"`
}

type PersistFileConfig struct {
	Path string `yaml:"path"` // empty = write back to the file given by -config
}

// PersistConfigMapConfig carries the ConfigMap coordinates that used to be hardcoded constants in
// internal/server/configwrite.go — defaults below match those constants exactly, so an existing
// deployment that never sets this block behaves identically.
type PersistConfigMapConfig struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
	Key       string `yaml:"key"`
}

// AuthConfig holds settings shared across every login on the /admin, /manager, and /dashboard
// web UIs. Individual accounts live in Users, not here (see User) — this only holds the
// session-signing sanity check.
type AuthConfig struct {
	// SessionSecret isn't used for cryptographic signing (sessions are server-side, keyed by an
	// opaque crypto/rand token — see internal/auth) — requiring it here is a deploy-time sanity
	// check that a real secrets bundle was provisioned before login is enabled, not left as an
	// accidental default. Required (>=32 bytes) if any User is configured.
	SessionSecret string `yaml:"session_secret"`
}

// User is one named login account for the /admin, /manager, or /viewer web UIs — granting
// or revoking an employee's access means adding/editing/removing their User entry, normally done
// through /admin's Access panel (config.WithUser/WithoutUser), not by hand-editing YAML after the
// first bootstrap admin account. Role gates which of the three doors (see internal/server/login.go
// roleUI) the account can log into: "admin" (full config surface), "manager" (status + wake-mode
// control), or "viewer" (status only, read-only). PasswordSHA256 follows the same hash
// pattern as Room.TokenSHA256 — not a slow salted hash, since these are IT-issued/rotated
// credentials, not end-user passwords chosen under attacker-guessable conditions.
type User struct {
	Username       string `yaml:"username"`
	PasswordSHA256 string `yaml:"password_sha256"`
	Role           string `yaml:"role"` // "admin" | "manager" | "viewer"

	// MustChangePassword is set whenever an admin sets or resets this account's password (see
	// handleAdminSaveUser) and cleared once the account holder picks their own password (see
	// handleChangePasswordSubmit). While true, requireRole redirects every request for this
	// session to the change-password page — the account can't be used for anything else until
	// the admin-chosen password is replaced.
	MustChangePassword bool `yaml:"must_change_password,omitempty"`

	// TOTPSecret is a base32 RFC 6238 secret — empty disables 2FA for this account. Self-enrolled
	// from the admin page (see handleTOTPSetupSubmit), not admin-set like PasswordSHA256, so one
	// admin can never see or set another admin's 2FA secret.
	TOTPSecret string `yaml:"totp_secret,omitempty"`
}

// FirmwareConfig drives OTA. The broker advertises Version+URL to devices via response headers;
// a device whose running build differs downloads the signed image at URL and reboots into it.
type FirmwareConfig struct {
	Version string `yaml:"version"` // target build, e.g. "1.1.0"; empty disables OTA advertising
	URL     string `yaml:"url"`     // HTTPS URL of the signed .bin (may point at this broker's /firmware/)
	Dir     string `yaml:"dir"`     // optional: serve image files from this dir at /firmware/

	// WebToolsURL is the ESP Web Tools module the /admin "Add a device" wizard loads to flash a
	// unit over WebSerial. Defaults to the public CDN build, which requires the *operator's
	// browser* (not the broker) to reach the internet. On an isolated network, vendor the file
	// into firmware.dir and point this at "/firmware/esp-web-tools.js" instead — the wizard
	// doesn't care where the module comes from.
	WebToolsURL string `yaml:"web_tools_url"`
}

// FleetConfig holds settings shared by every display, set once instead of per device. Today that
// is just the Wi-Fi credentials the provisioning wizard bakes into each unit's NVS: making them a
// single fleet-wide value is the fix for hand-typing a stale SSID into one device at a time.
//
// WifiPSK is a secret and follows the same ${ENV_VAR} indirection as AlertConfig.WebhookURL —
// keep it as "${MD_WIFI_PSK}" in the ConfigMap and inject the real value from the broker-secrets
// Secret, so Load expands it in memory and MarshalForPersist restores the token before any admin
// save writes the config back out. It is never served to a browser; see internal/nvs.
type FleetConfig struct {
	WifiSSID string `yaml:"wifi_ssid"`
	WifiPSK  string `yaml:"wifi_psk"`
}

// AlertConfig drives the in-broker low-battery notification to the building manager, and doubles
// as the single source of truth for the thresholds the status endpoint (internal/status) uses to
// classify a device — so /api/v1/status and the Prometheus rules in deploy/k3s/alerts.yaml never
// disagree about what counts as "low battery" or "stale".
// (The fleet-grade alternative is a Prometheus alert on md_battery_percent — see
// deploy/k3s/alerts.yaml. The two are complementary; you can run either or both.)
type AlertConfig struct {
	LowBatteryPct int           `yaml:"low_battery_pct"` // fire at/below this percent (default 45)
	ClearPct      int           `yaml:"clear_pct"`       // reset alert state at/above this (default 55, hysteresis)
	MinRenotify   time.Duration `yaml:"min_renotify"`    // suppress repeat alerts within this window (default 24h)
	WebhookURL    string        `yaml:"webhook_url"`     // Slack incoming webhook; empty = log only
	StaleAfter    time.Duration `yaml:"stale_after"`     // no telemetry for this long = "stale" (default 1h, matches DisplayStale)
}

type WakeConfig struct {
	// Mode is the fleet-wide default wake strategy — "flat" (fixed interval, grid-aligned) or
	// "smart" (calendar-driven: wake only when the room's displayed status would actually
	// change, plus a periodic safety-net check-in). A per-room Room.WakeMode overrides this.
	// Empty ("") behaves as "flat" — an explicit opt-in is required for smart mode so existing
	// deployments don't change behavior on upgrade.
	Mode string `yaml:"mode"`

	// BusinessHoursSeconds serves two roles depending on mode: in "flat" mode it's the fixed
	// grid-aligned check-in interval during business hours; in "smart" mode it's both the
	// periodic safety-net interval (when nothing is on the calendar) and the hard cap on how
	// long to sleep even when a next transition is known further out — meetings get
	// created/moved/cancelled live during the day, so business hours never fully trusts the
	// calendar the way off-hours does.
	BusinessHoursSeconds uint32 `yaml:"business_hours_seconds"`
	// OffHoursSeconds is the "flat" mode off-hours interval. Unused in "smart" mode: off-hours
	// there is uncapped — sleep until the next known event, or straight through to the next
	// business-hours start if nothing is scheduled.
	OffHoursSeconds   uint32 `yaml:"off_hours_seconds"`
	BusinessStartHour int    `yaml:"business_start_hour"`
	BusinessEndHour   int    `yaml:"business_end_hour"`
	Timezone          string `yaml:"timezone"`

	// ForcedRefreshHour: local hour (0-23) at which each device gets one unconditional full-panel
	// repaint per day, even if the room schedule hasn't changed. E-ink retains a faint bias from
	// sitting on the same image across many wake cycles; a periodic repaint (the panel already
	// does a full, not partial, refresh — see firmware/main/epd_spectra6.cpp) clears it before it
	// becomes visible. Pointer so "unset" (feature off) is distinguishable from hour 0 (midnight).
	// Pick an off-hours value (outside BusinessStartHour..BusinessEndHour) so it doesn't cost
	// energy during the work day. nil = disabled (default).
	ForcedRefreshHour *int `yaml:"forced_refresh_hour"`
}

type RenderConfig struct {
	Width  int  `yaml:"width"`
	Height int  `yaml:"height"`
	Dither bool `yaml:"dither"`
}

type GoogleConfig struct {
	// Path to the external-account (Workload Identity Federation) credential config JSON.
	// KEYLESS — references the broker's projected k8s token; no service-account key.
	// Empty → fall back to GOOGLE_APPLICATION_CREDENTIALS (ADC). No impersonation subject:
	// the service account reads rooms shared to it directly (freeBusyReader).
	CredentialsFile string `yaml:"credentials_file"`
}

// Room is one *display*, not one meeting room — several entries may share the same Room
// (calendar address) when a room has more than one panel (e.g. a door plaque plus an interior
// display). DeviceID is the unique key; Room is deliberately not unique. When it repeats, Label
// is what tells the two panels apart everywhere a human looks (see Label, and status.Device).
type Room struct {
	DeviceID    string `yaml:"device_id"`
	Name        string `yaml:"name"`
	Room        string `yaml:"room"`
	TokenSHA256 string `yaml:"token_sha256"`

	// Label distinguishes multiple displays serving the same Room — the panel's physical
	// position ("Door", "Interior wall"), not the room's name. Optional and empty for the
	// overwhelmingly common one-display-per-room case; required (and unique) once two entries
	// share a Room, enforced in Validate, so a fleet can never contain two indistinguishable
	// cards on /dashboard. It affects presentation only: both panels in a room render the same
	// image, since Compose keys off the schedule and Name.
	Label string `yaml:"label,omitempty"`

	// WakeMode overrides WakeConfig.Mode for this room only — e.g. a room whose occupant just
	// wants a plain 15-min check-in without calendar-driven scheduling. nil = use fleet default.
	WakeMode *string `yaml:"wake_mode,omitempty"`
	// FlatIntervalSeconds overrides the fixed check-in interval when this room's effective mode
	// is "flat" — grid-aligned regardless of business/off hours. nil = use the fleet's
	// business/off-hours split (WakeConfig.BusinessHoursSeconds/OffHoursSeconds).
	FlatIntervalSeconds *uint32 `yaml:"flat_interval_seconds,omitempty"`
}

var envRef = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// minEnvRefLen is the shortest expanded value Load will remember for MarshalForPersist to restore
// — anything shorter risks a coincidental substring match elsewhere in the marshaled YAML (e.g. a
// 2-character env value that also happens to appear inside an unrelated field), rewriting text
// that was never a secret in the first place.
const minEnvRefLen = 8

// Load reads YAML, expands ${ENV} references, and validates. Every expansion actually performed is
// remembered on the returned Config (see envRefs) so a later MarshalForPersist can put the ${VAR}
// token back instead of writing the expanded secret to disk or into a ConfigMap.
//
// A ${VAR} that expands to nothing is a hard error, not an empty string. This is load-bearing, and
// the reason is not obvious: an unset variable produced no envRefs entry, so MarshalForPersist had
// no token to restore, so the first admin save wrote "" straight over the ${VAR} in the ConfigMap —
// silently and permanently destroying the reference. Restoring the Secret key afterwards did not
// undo it; someone had to know to hand-edit the reference back. That is not hypothetical: the live
// cluster's alerts.webhook_url is "" today with its Secret key absent, and it went unnoticed
// precisely because an empty webhook is harmless (it degrades to log-only).
//
// Recording the ref anyway is not the fix — it is worse. MarshalForPersist substitutes by
// bytes.ReplaceAll(out, expanded, token), and an empty `expanded` splices the token between every
// byte of the document. Empty is a degenerate case the substring design cannot express at all.
//
// A dangling ref is always a deployment error: "legitimately empty" is already expressible by
// writing `webhook_url: ""`. Naming a variable that does not exist is a way of saying "a secret
// belongs here and is missing", so failing at boot turns silent permanent corruption into a loud,
// trivially-diagnosed startup error.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	envRefs := map[string]string{}
	// fromEnv tracks every value that came from an expansion, including ones too short for
	// envRefs' substitution filter. Only used to tell a literal from an expanded ref when
	// warning about plaintext secrets below — never for substitution.
	fromEnv := map[string]bool{}
	raw = envRef.ReplaceAllFunc(raw, func(m []byte) []byte {
		key := string(envRef.FindSubmatch(m)[1])
		val, ok := os.LookupEnv(key)
		if !ok || val == "" {
			return m // leave the token intact; detected as dangling after parsing, below
		}
		fromEnv[val] = true
		if len(val) >= minEnvRefLen {
			envRefs[val] = string(m) // m is the whole "${VAR}" token
		}
		return []byte(val)
	})

	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Dangling refs are detected AFTER parsing, not during substitution above. The regex runs over
	// raw bytes, so it also matches ${VAR} written inside a YAML comment — and this file documents
	// the mechanism in its own comments ("keep it as ${MD_WIFI_PSK} and inject from a Secret").
	// Erroring during substitution would make a config unstartable because of its own documentation.
	// Re-marshalling the parsed struct drops every comment, so whatever ${...} survives here is a
	// reference that landed in a real configuration value.
	if remarshalled, err := yaml.Marshal(&c); err == nil {
		if dangling := envRef.FindAllSubmatch(remarshalled, -1); len(dangling) > 0 {
			seen := map[string]bool{}
			var missing []string
			for _, d := range dangling {
				if k := string(d[1]); !seen[k] {
					seen[k] = true
					missing = append(missing, k)
				}
			}
			sort.Strings(missing) // deterministic message regardless of field order
			return nil, fmt.Errorf("config %s references environment variable(s) that are unset or "+
				"empty: %s — set them, or write a literal value (e.g. `webhook_url: \"\"`) if the "+
				"field is genuinely meant to be empty. Starting with the reference unexpanded would "+
				"let the next admin save overwrite it permanently", path, strings.Join(missing, ", "))
		}
	}

	c.envRefs = envRefs
	c.warnPlaintextSecrets(fromEnv)
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// MarshalForPersist marshals c for write-back with ${ENV} references restored. Load expands
// ${VAR} in memory; marshalling the expanded value would bake a secret out of the Secret and
// into the ConfigMap (or, worse, an on-disk config file) on the first admin save. Skips
// expansions shorter than minEnvRefLen (already excluded when envRefs was built) — nothing further
// to do here beyond the substitution itself.
// Substitution runs longest-expanded-value-first. Go randomizes map iteration order, so if one
// expanded value is a substring of another, iterating the map directly would make the output depend
// on run-to-run ordering — the same Config could marshal two different ways, and the shorter value
// could chew a hole in the longer one before it was ever matched. Longest-first is the standard
// fix and makes the result deterministic.
func (c *Config) MarshalForPersist() ([]byte, error) {
	out, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	expanded := make([]string, 0, len(c.envRefs))
	for v := range c.envRefs {
		expanded = append(expanded, v)
	}
	sort.Slice(expanded, func(i, j int) bool {
		if len(expanded[i]) != len(expanded[j]) {
			return len(expanded[i]) > len(expanded[j])
		}
		return expanded[i] < expanded[j] // stable tiebreak for equal-length values
	})
	for _, v := range expanded {
		out = bytes.ReplaceAll(out, []byte(v), []byte(c.envRefs[v]))
	}
	return out, nil
}

// secretFields are the values that must never be written back as plaintext. Kept as one list so
// adding a secret field to Config is a one-line change here rather than a silent omission.
func (c *Config) secretFields() map[string]string {
	return map[string]string{
		"auth.session_secret": c.Auth.SessionSecret,
		"alerts.webhook_url":  c.Alerts.WebhookURL,
		"fleet.wifi_psk":      c.Fleet.WifiPSK,
	}
}

// warnPlaintextSecrets logs when a secret field holds a literal rather than a ${VAR} reference.
// The ${VAR} mechanism is opt-in per value and silent when unused, which is exactly how a plaintext
// session_secret reached the live ConfigMap in the first place: nothing anywhere said "this is a
// secret and you wrote it in the clear." A literal still works, so this warns rather than refusing —
// but it now says so out loud, once, at startup.
//
// Values shorter than minEnvRefLen that DID come from the environment are still flagged, since
// fromEnv records every expansion regardless of length. A secret that short is its own problem.
func (c *Config) warnPlaintextSecrets(fromEnv map[string]bool) {
	for name, val := range c.secretFields() {
		if val == "" || fromEnv[val] {
			continue
		}
		slog.Warn("secret is a literal in the config file, not a ${VAR} reference — it will be "+
			"written back in plaintext on every admin save; move it to an env var/Secret and "+
			"reference it instead", "field", name)
	}
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.PollInterval == 0 {
		c.PollInterval = 2 * time.Minute
	}
	if c.Render.Width == 0 {
		c.Render.Width = 800
	}
	if c.Render.Height == 0 {
		c.Render.Height = 480
	}
	if c.Wake.BusinessHoursSeconds == 0 {
		c.Wake.BusinessHoursSeconds = 600
	}
	if c.Wake.OffHoursSeconds == 0 {
		c.Wake.OffHoursSeconds = 3600
	}
	if c.Wake.Timezone == "" {
		c.Wake.Timezone = "UTC"
	}
	// Default to a 45% early-buffer threshold: LiPo voltage→percent is nonlinear, so a flat
	// threshold off the firmware's linear voltage map is approximate — alert a little early.
	if c.Alerts.LowBatteryPct == 0 {
		c.Alerts.LowBatteryPct = 45
	}
	if c.Alerts.ClearPct == 0 {
		c.Alerts.ClearPct = c.Alerts.LowBatteryPct + 10 // hysteresis band
	}
	if c.Alerts.MinRenotify == 0 {
		c.Alerts.MinRenotify = 24 * time.Hour
	}
	if c.Alerts.StaleAfter == 0 {
		c.Alerts.StaleAfter = time.Hour
	}
	if c.Telemetry.Backend == "" {
		c.Telemetry.Backend = "memory"
	}
	if c.Telemetry.SQLite.Path == "" {
		c.Telemetry.SQLite.Path = "/var/lib/meeting-displays/telemetry.db"
	}
	if c.Telemetry.SQLite.RetentionDays == 0 {
		c.Telemetry.SQLite.RetentionDays = 90
	}
	if c.Telemetry.SQLite.PruneInterval == 0 {
		c.Telemetry.SQLite.PruneInterval = 6 * time.Hour
	}
	if c.Telemetry.SQLite.QueueSize == 0 {
		c.Telemetry.SQLite.QueueSize = 256
	}
	if c.ConfigPersistence.Mode == "" {
		c.ConfigPersistence.Mode = "auto"
	}
	// Pinned to a major version rather than "latest": an unattended upgrade of a third-party
	// module that drives a flash tool is not something to discover mid-provisioning.
	if c.Firmware.WebToolsURL == "" {
		c.Firmware.WebToolsURL = "https://unpkg.com/esp-web-tools@10/dist/web/install-button.js"
	}
	// These three match today's hardcoded constants in internal/server/configwrite.go exactly, so
	// an existing config.yaml that never mentions config_persistence behaves identically.
	if c.ConfigPersistence.ConfigMap.Namespace == "" {
		c.ConfigPersistence.ConfigMap.Namespace = "meeting-displays"
	}
	if c.ConfigPersistence.ConfigMap.Name == "" {
		c.ConfigPersistence.ConfigMap.Name = "broker-config"
	}
	if c.ConfigPersistence.ConfigMap.Key == "" {
		c.ConfigPersistence.ConfigMap.Key = "config.yaml"
	}
}

// Validate checks the config for internal consistency. It's called once at Load time, and again
// on every admin/manager write (see WithRoom, WithoutRoom, WithRoomWakeOverride) so a bad edit
// from the web UI can never produce an in-memory or persisted config that would fail to load on
// the next restart.
func (c *Config) Validate() error {
	if c.Provider != "google" && c.Provider != "demo" {
		return fmt.Errorf("provider must be 'google' or 'demo', got %q", c.Provider)
	}
	if len(c.Rooms) == 0 {
		return fmt.Errorf("no rooms configured")
	}
	seen := map[string]bool{}
	for i, r := range c.Rooms {
		if r.DeviceID == "" || r.Room == "" {
			return fmt.Errorf("room[%d]: device_id and room are required", i)
		}
		if seen[r.DeviceID] {
			return fmt.Errorf("duplicate device_id %q", r.DeviceID)
		}
		seen[r.DeviceID] = true
		if len(r.TokenSHA256) != 64 {
			return fmt.Errorf("room %q: token_sha256 must be 64 hex chars", r.DeviceID)
		}
		if r.WakeMode != nil && !validWakeMode(*r.WakeMode) {
			return fmt.Errorf("room %q: wake_mode must be '', 'flat', or 'smart', got %q", r.DeviceID, *r.WakeMode)
		}
	}
	if err := validateRoomLabels(c.Rooms); err != nil {
		return err
	}
	if _, err := time.LoadLocation(c.Wake.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Wake.Timezone, err)
	}
	if h := c.Wake.ForcedRefreshHour; h != nil && (*h < 0 || *h > 23) {
		return fmt.Errorf("wake.forced_refresh_hour must be 0-23, got %d", *h)
	}
	if !validWakeMode(c.Wake.Mode) {
		return fmt.Errorf("wake.mode must be '', 'flat', or 'smart', got %q", c.Wake.Mode)
	}
	if !validTelemetryBackend(c.Telemetry.Backend) {
		return fmt.Errorf("telemetry.backend must be '', 'memory', 'sqlite', or 'prometheus', got %q", c.Telemetry.Backend)
	}
	if !validConfigPersistenceMode(c.ConfigPersistence.Mode) {
		return fmt.Errorf("config_persistence.mode must be '', 'auto', 'file', 'configmap', or 'none', got %q", c.ConfigPersistence.Mode)
	}
	if err := c.validateUsers(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateUsers() error {
	if len(c.Users) == 0 {
		return nil // login disabled entirely — fails closed, not an error
	}
	if len(c.Auth.SessionSecret) < 32 {
		return fmt.Errorf("auth.session_secret must be set (>=32 bytes) when any user is configured")
	}
	seenUsername := map[string]bool{}
	hasAdmin := false
	for i, u := range c.Users {
		if u.Username == "" {
			return fmt.Errorf("user[%d]: username is required", i)
		}
		key := strings.ToLower(u.Username)
		if seenUsername[key] {
			return fmt.Errorf("duplicate username %q (usernames are case-insensitive)", u.Username)
		}
		seenUsername[key] = true
		if len(u.PasswordSHA256) != 64 {
			return fmt.Errorf("user %q: password_sha256 must be 64 hex chars", u.Username)
		}
		if !validUserRole(u.Role) {
			return fmt.Errorf("user %q: role must be 'admin', 'manager', or 'viewer', got %q", u.Username, u.Role)
		}
		if u.Role == "admin" {
			hasAdmin = true
		}
	}
	// Without this, deleting/demoting the last admin through /admin would lock every future
	// change out of the UI — the only remaining fix would be hand-editing the ConfigMap.
	if !hasAdmin {
		return fmt.Errorf("at least one user must have role 'admin'")
	}
	return nil
}

// Location returns the configured business timezone (validated at load).
func (c *Config) Location() *time.Location {
	loc, _ := time.LoadLocation(c.Wake.Timezone)
	return loc
}

// validateRoomLabels enforces Room.Label only where it actually matters: a room address served by
// two or more displays. A single-display room may leave Label empty forever (and every config
// written before multi-display support keeps loading unchanged); the moment a second panel is
// added for the same address, both need a non-empty, distinct label, because otherwise /dashboard
// and /status would show two identically-titled cards with no way to tell which physical panel is
// stale or flat.
//
// Comparison is case-insensitive on both the room address (Google treats addresses that way) and
// the label, so "Door" and "door" collide rather than producing two cards a human reads as one.
func validateRoomLabels(rooms []Room) error {
	byRoom := map[string][]Room{}
	for _, r := range rooms {
		key := strings.ToLower(r.Room)
		byRoom[key] = append(byRoom[key], r)
	}
	for _, group := range byRoom {
		if len(group) < 2 {
			continue
		}
		seenLabel := map[string]string{} // lowercased label -> device_id that claimed it
		for _, r := range group {
			if r.Label == "" {
				return fmt.Errorf("room %q is served by %d displays, so room %q needs a label "+
					"(e.g. \"Door\", \"Interior wall\") to tell them apart", r.Room, len(group), r.DeviceID)
			}
			key := strings.ToLower(r.Label)
			if other, dup := seenLabel[key]; dup {
				return fmt.Errorf("rooms %q and %q both serve %q with the label %q; labels must be distinct",
					other, r.DeviceID, r.Room, r.Label)
			}
			seenLabel[key] = r.DeviceID
		}
	}
	return nil
}

// RoomByDeviceID finds a room's config by device ID — a linear scan is fine at fleet sizes this
// project targets (see poller.go's own bounded-fan-out comment).
func (c *Config) RoomByDeviceID(deviceID string) (Room, bool) {
	for _, r := range c.Rooms {
		if r.DeviceID == deviceID {
			return r, true
		}
	}
	return Room{}, false
}

// WithRoom returns a deep copy of c with room updated.DeviceID replaced (if it already exists) or
// appended (if deviceID is new), validated before being returned. This is the only way the
// admin UI's room add/edit forms mutate config — callers never touch c.Rooms directly, so a bad
// edit can't reach Live.Store without first failing Validate.
func (c *Config) WithRoom(updated Room) (*Config, error) {
	next := c.clone()
	replaced := false
	for i, r := range next.Rooms {
		if r.DeviceID == updated.DeviceID {
			next.Rooms[i] = updated
			replaced = true
			break
		}
	}
	if !replaced {
		next.Rooms = append(next.Rooms, updated)
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithoutRoom returns a deep copy of c with room deviceID removed, validated before being
// returned (e.g. rejects deleting the last remaining room).
func (c *Config) WithoutRoom(deviceID string) (*Config, error) {
	next := c.clone()
	out := make([]Room, 0, len(next.Rooms))
	for _, r := range next.Rooms {
		if r.DeviceID != deviceID {
			out = append(out, r)
		}
	}
	next.Rooms = out
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithRoomWakeOverride returns a deep copy of c with room deviceID's WakeMode/FlatIntervalSeconds
// overridden, validated before being returned. This is the only write the /manager UI performs.
func (c *Config) WithRoomWakeOverride(deviceID string, mode *string, flatSeconds *uint32) (*Config, error) {
	next := c.clone()
	found := false
	for i, r := range next.Rooms {
		if r.DeviceID == deviceID {
			next.Rooms[i].WakeMode = mode
			next.Rooms[i].FlatIntervalSeconds = flatSeconds
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("room %q not found", deviceID)
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithWake returns a deep copy of c with the fleet-wide wake defaults replaced, validated before
// being returned. Used by the admin UI's wake-defaults form.
func (c *Config) WithWake(w WakeConfig) (*Config, error) {
	next := c.clone()
	next.Wake = w
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithAlerts returns a deep copy of c with AlertConfig replaced, validated before being returned.
func (c *Config) WithAlerts(a AlertConfig) (*Config, error) {
	next := c.clone()
	next.Alerts = a
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithFirmware returns a deep copy of c with FirmwareConfig replaced, validated before being
// returned.
func (c *Config) WithFirmware(f FirmwareConfig) (*Config, error) {
	next := c.clone()
	next.Firmware = f
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithFleetWifiSSID returns a deep copy of c with the fleet Wi-Fi SSID replaced, validated before
// being returned. Only the SSID: the PSK is a secret sourced from ${MD_WIFI_PSK} and is never
// settable from a web form, so this deliberately carries the existing WifiPSK forward untouched
// rather than accepting one from a caller.
func (c *Config) WithFleetWifiSSID(ssid string) (*Config, error) {
	next := c.clone()
	next.Fleet.WifiSSID = ssid
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithUser returns a deep copy of c with a user matching updated.Username (case-insensitive)
// replaced, or appended if new, validated before being returned. This is the only way /admin's
// Access panel grants or edits an employee's login — callers never touch c.Users directly.
// updated.PasswordSHA256 must always be set by the caller (on edit-without-changing-password,
// the caller is responsible for carrying the existing hash forward, mirroring WithRoom's token
// handling in internal/server/admin.go).
func (c *Config) WithUser(updated User) (*Config, error) {
	next := c.clone()
	replaced := false
	for i, u := range next.Users {
		if strings.EqualFold(u.Username, updated.Username) {
			next.Users[i] = updated
			replaced = true
			break
		}
	}
	if !replaced {
		next.Users = append(next.Users, updated)
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithoutUser returns a deep copy of c with the user matching username (case-insensitive)
// removed, validated before being returned (e.g. rejects removing the last admin account).
func (c *Config) WithoutUser(username string) (*Config, error) {
	next := c.clone()
	out := make([]User, 0, len(next.Users))
	for _, u := range next.Users {
		if !strings.EqualFold(u.Username, username) {
			out = append(out, u)
		}
	}
	next.Users = out
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// UserByUsername finds a user's config by username, case-insensitive.
func (c *Config) UserByUsername(username string) (User, bool) {
	for _, u := range c.Users {
		if strings.EqualFold(u.Username, username) {
			return u, true
		}
	}
	return User{}, false
}

// clone returns a shallow struct copy of c with Rooms and Users deep-copied into fresh slices, so
// mutating the copy's slices (element replacement or append) never aliases the original's backing
// array. Per-room pointer fields (WakeMode, FlatIntervalSeconds) are replaced wholesale by callers
// above rather than mutated in place, so copying the pointers themselves is safe. `next := *c`
// already carries envRefs forward (a plain field copy of the map reference) — safe because envRefs
// is written once by Load and never mutated afterward, only read by MarshalForPersist.
func (c *Config) clone() *Config {
	next := *c
	next.Rooms = append([]Room(nil), c.Rooms...)
	next.Users = append([]User(nil), c.Users...)
	return &next
}

func validWakeMode(m string) bool {
	return m == "" || m == "flat" || m == "smart"
}

func validUserRole(r string) bool {
	return r == "admin" || r == "manager" || r == "viewer"
}

// validTelemetryBackend treats "" as valid (same convention as validWakeMode above) — a bare
// Config{} that hasn't been through applyDefaults yet (e.g. in tests, or mid-construction) is still
// a legal value; applyDefaults is what turns "" into "memory" before the broker actually starts.
func validTelemetryBackend(b string) bool {
	return b == "" || b == "memory" || b == "sqlite" || b == "prometheus"
}

// validConfigPersistenceMode treats "" as valid for the same reason validTelemetryBackend does.
func validConfigPersistenceMode(m string) bool {
	return m == "" || m == "auto" || m == "file" || m == "configmap" || m == "none"
}

// wakeModeFor resolves the effective wake mode for a room: its own override if set, else the
// fleet default, else "flat" — an explicit "smart" opt-in is required so existing deployments
// don't change behavior on upgrade.
func (c *Config) wakeModeFor(room Room) string {
	if room.WakeMode != nil && *room.WakeMode != "" {
		return *room.WakeMode
	}
	if c.Wake.Mode != "" {
		return c.Wake.Mode
	}
	return "flat"
}

// TEMPORARY (2026-07-19): weekday restriction below is disabled for weekend testing —
// REVERT after testing by restoring `wd != time.Saturday && wd != time.Sunday &&` below.
//
// isBusinessHours reports whether now (any timezone) falls within the configured business
// window in the fleet's local timezone. Shared by both wake modes so they never disagree about
// what counts as "business hours".
func (c *Config) isBusinessHours(now time.Time) bool {
	h := now.In(c.Location()).Hour()
	return h >= c.Wake.BusinessStartHour && h < c.Wake.BusinessEndHour
}

// NextWakeDuration returns how long device room's owner should sleep, given its effective wake
// mode and (for smart mode) the calendar state resolved by the poller. cur/next may be up to
// PollInterval stale as *which* events they point to, but the events' own Start/End timestamps
// are exact calendar data — recomputing the duration against a fresh `now` here is always
// correct even though the snapshot was taken up to PollInterval ago.
func (c *Config) NextWakeDuration(room Room, cur, next *calendar.Event, now time.Time) uint32 {
	if c.wakeModeFor(room) == "smart" {
		return c.smartWakeSeconds(cur, next, now)
	}
	return c.flatWakeSeconds(room, now)
}

// flatWakeSeconds is the fixed-interval, grid-aligned strategy — lands on a fixed clock grid
// (e.g. :00/:15/:30/:45 for a 900s interval, offset by gridOffsetSeconds) rather than counting
// forward from whenever this particular device last checked in, so every device converges on the
// same wall-clock wake times and a device that missed a wake rejoins the same grid next time.
func (c *Config) flatWakeSeconds(room Room, now time.Time) uint32 {
	local := now.In(c.Location())
	if room.FlatIntervalSeconds != nil && *room.FlatIntervalSeconds > 0 {
		return secondsUntilNextBoundary(local, *room.FlatIntervalSeconds)
	}
	interval := c.Wake.OffHoursSeconds
	if c.isBusinessHours(now) {
		interval = c.Wake.BusinessHoursSeconds
	}
	return secondsUntilNextBoundary(local, interval)
}

// smartWakeSeconds wakes only when the room's displayed status would actually change (per
// calendar.NextTransitionAt), with a business-hours safety-net cap (meetings get
// created/moved/cancelled live during the day) and no cap off-hours (trusting the calendar,
// since off-hours activity is rare) — falling back to a periodic check or a sleep-through to the
// next business-hours start when nothing is on the calendar at all.
func (c *Config) smartWakeSeconds(cur, next *calendar.Event, now time.Time) uint32 {
	business := c.isBusinessHours(now)

	transitionAt, ok := calendar.NextTransitionAt(cur, next, now)
	if !ok {
		if business {
			return c.Wake.BusinessHoursSeconds // periodic safety-net check-in; cheap if unchanged
		}
		return secondsUntilBusinessHoursStart(now.In(c.Location()), c.Wake.BusinessStartHour)
	}

	dur := transitionAt.Sub(now)
	if dur < 0 {
		dur = 0
	}
	if business && uint32(dur.Seconds()) > c.Wake.BusinessHoursSeconds {
		return c.Wake.BusinessHoursSeconds
	}
	return uint32(dur.Seconds())
}

// secondsUntilBusinessHoursStart returns the duration until the next occurrence of
// businessStartHour:00 in local's timezone (today if not yet past that hour, else tomorrow).
func secondsUntilBusinessHoursStart(local time.Time, businessStartHour int) uint32 {
	next := time.Date(local.Year(), local.Month(), local.Day(), businessStartHour, 0, 0, 0, local.Location())
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return uint32(next.Sub(local).Seconds())
}

// gridOffsetSeconds shifts the wake grid 5 minutes past each boundary (e.g. :05/:20/:35/:50 for a
// 900s interval instead of :00/:15/:30/:45) so a check-in trails a meeting's start/end time rather
// than racing it — calendar events conventionally start exactly on the hour/quarter-hour, and a
// device waking at the identical instant risks reading the schedule right at the edge of a
// transition.
const gridOffsetSeconds = 5 * 60

// secondsUntilNextBoundary returns how long until the next multiple of intervalSeconds since the
// Unix epoch, shifted by gridOffsetSeconds — e.g. interval=900 lands on :05/:20/:35/:50 past the
// hour (in any timezone with a whole-hour UTC offset, which covers every zone this fleet runs in).
func secondsUntilNextBoundary(now time.Time, intervalSeconds uint32) uint32 {
	if intervalSeconds == 0 {
		return 0
	}
	interval := int64(intervalSeconds)
	epoch := now.Unix() - gridOffsetSeconds
	next := (epoch/interval + 1) * interval
	return uint32(next + gridOffsetSeconds - now.Unix())
}
