// Package config loads and validates broker configuration from YAML + environment.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen       string         `yaml:"listen"`
	PollInterval time.Duration  `yaml:"poll_interval"`
	Wake         WakeConfig     `yaml:"wake"`
	Render       RenderConfig   `yaml:"render"`
	Provider     string         `yaml:"provider"`
	Google       GoogleConfig   `yaml:"google"`
	Alerts       AlertConfig    `yaml:"alerts"`
	Firmware     FirmwareConfig `yaml:"firmware"`
	Rooms        []Room         `yaml:"rooms"`
}

// FirmwareConfig drives OTA. The broker advertises Version+URL to devices via response headers;
// a device whose running build differs downloads the signed image at URL and reboots into it.
type FirmwareConfig struct {
	Version string `yaml:"version"` // target build, e.g. "1.1.0"; empty disables OTA advertising
	URL     string `yaml:"url"`     // HTTPS URL of the signed .bin (may point at this broker's /firmware/)
	Dir     string `yaml:"dir"`     // optional: serve image files from this dir at /firmware/
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
	BusinessHoursSeconds uint32 `yaml:"business_hours_seconds"`
	OffHoursSeconds      uint32 `yaml:"off_hours_seconds"`
	BusinessStartHour    int    `yaml:"business_start_hour"`
	BusinessEndHour      int    `yaml:"business_end_hour"`
	Timezone             string `yaml:"timezone"`

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

type Room struct {
	DeviceID    string `yaml:"device_id"`
	Name        string `yaml:"name"`
	Room        string `yaml:"room"`
	TokenSHA256 string `yaml:"token_sha256"`
}

var envRef = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// Load reads YAML, expands ${ENV} references, and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	raw = envRef.ReplaceAllFunc(raw, func(m []byte) []byte {
		key := envRef.FindSubmatch(m)[1]
		return []byte(os.Getenv(string(key)))
	})

	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
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
}

func (c *Config) validate() error {
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
	}
	if _, err := time.LoadLocation(c.Wake.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Wake.Timezone, err)
	}
	if h := c.Wake.ForcedRefreshHour; h != nil && (*h < 0 || *h > 23) {
		return fmt.Errorf("wake.forced_refresh_hour must be 0-23, got %d", *h)
	}
	return nil
}

// Location returns the configured business timezone (validated at load).
func (c *Config) Location() *time.Location {
	loc, _ := time.LoadLocation(c.Wake.Timezone)
	return loc
}

// NextWakeSeconds returns the recommended sleep duration for a device given the wall clock now.
func (c *Config) NextWakeSeconds(now time.Time) uint32 {
	now = now.In(c.Location())
	h := now.Hour()
	wd := now.Weekday()
	business := wd != time.Saturday && wd != time.Sunday &&
		h >= c.Wake.BusinessStartHour && h < c.Wake.BusinessEndHour
	if business {
		return c.Wake.BusinessHoursSeconds
	}
	return c.Wake.OffHoursSeconds
}
