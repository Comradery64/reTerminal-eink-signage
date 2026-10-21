package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Notification channels an AlertConfig can select. Each one's secret follows the ${ENV_VAR}
// indirection (see Load) and is checked fail-closed by validateNotify: selecting a channel whose
// secret is unset is a startup error, not a silent fall-back to log-only.
const (
	ChannelLark    = "lark"    // Lark custom app: DM to each location's person — alerts.lark_app (secret ${LARK_APP_SECRET})
	ChannelWebPush = "webpush" // browser push to subscribed admins/managers — alerts.webpush.*
	ChannelSlack   = "slack"   // legacy Slack incoming webhook — alerts.webhook_url
	// ChannelLog is an explicit "no channels": alerts are logged only. Needed because an empty
	// list means legacy behavior (Slack whenever webhook_url is set), so turning every channel off
	// in /admin must be expressible without deleting the webhook's ${VAR} reference.
	ChannelLog = "log"
)

// WebPushConfig is the VAPID identity the broker signs pushes with. The private key is a secret
// (${WEBPUSH_VAPID_PRIVATE_KEY}); the public key is not, but uses the same indirection so both
// halves of the pair come from one Secret and can't drift.
type WebPushConfig struct {
	VAPIDPublicKey  string `yaml:"vapid_public_key,omitempty"`  // base64url, 65-byte uncompressed P-256 point
	VAPIDPrivateKey string `yaml:"vapid_private_key,omitempty"` // base64url, 32-byte P-256 scalar
	// Subject is the VAPID "sub" claim: a mailto: or https: contact the push services (Google,
	// Mozilla, Apple) can reach if this sender misbehaves. Required for webpush.
	Subject string `yaml:"subject,omitempty"`
}

// PushSubscription is one browser's Web Push subscription, created when a logged-in admin or
// manager clicks "Enable notifications on this browser". Persisted through the normal
// config-persistence path so it survives restarts; removed when the user turns it off or the
// push service reports it gone (404/410).
type PushSubscription struct {
	Username string    `yaml:"username"`
	Location string    `yaml:"location,omitempty"` // only this location's alerts reach it; "" = every location
	Endpoint string    `yaml:"endpoint"`
	P256DH   string    `yaml:"p256dh"`
	Auth     string    `yaml:"auth"`
	Created  time.Time `yaml:"created"`
}

// EffectiveChannels resolves the channel list. A non-empty alerts.channels wins. When it's
// empty, a config written before channels existed keeps its old behavior: Slack if
// webhook_url is set, otherwise log-only. (Empty rather than absent, because YAML omitempty
// can't round-trip the difference through an admin save.)
func (a AlertConfig) EffectiveChannels() []string {
	if len(a.Channels) > 0 {
		return a.Channels
	}
	if a.WebhookURL != "" {
		return []string{ChannelSlack}
	}
	return nil
}

func (c *Config) validateNotify() error {
	a := c.Alerts
	seen := map[string]bool{}
	for _, ch := range a.Channels {
		if seen[ch] {
			return fmt.Errorf("alerts.channels: %q listed twice", ch)
		}
		seen[ch] = true
		switch ch {
		case ChannelLark:
			if err := c.validateLarkApp(); err != nil {
				return fmt.Errorf("alerts.channels selects lark: %w", err)
			}
		case ChannelWebPush:
			if err := validateVAPID(a.WebPush); err != nil {
				return fmt.Errorf("alerts.channels selects webpush: %w", err)
			}
		case ChannelSlack:
			if err := requireHTTPS("alerts.webhook_url", a.WebhookURL); err != nil {
				return fmt.Errorf("alerts.channels selects slack: %w", err)
			}
		case ChannelLog:
			if len(a.Channels) != 1 {
				return fmt.Errorf("alerts.channels: \"log\" means no channels and can't be combined with others")
			}
		default:
			return fmt.Errorf("alerts.channels: unknown channel %q (want lark, webpush, slack, or log)", ch)
		}
	}
	for i, s := range c.PushSubscriptions {
		if err := validatePushSubscription(s); err != nil {
			return fmt.Errorf("push_subscriptions[%d]: %w", i, err)
		}
		if s.Location != "" {
			if _, ok := c.LocationByID(s.Location); !ok {
				return fmt.Errorf("push_subscriptions[%d]: unknown location %q", i, s.Location)
			}
		}
	}
	return c.validateLocations()
}

func requireHTTPS(field, v string) error {
	if v == "" {
		return fmt.Errorf("%s is empty — set its secret (see the ${ENV_VAR} note in config.example.yaml)", field)
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s must be an https:// URL", field)
	}
	return nil
}

// validateVAPID checks shape only (lengths, encoding, subject). Whether the private key really
// matches the public key is checked when the notifier is built (notify.ParseVAPIDKeys), which
// main runs before serving — still at startup, still fail-closed.
func validateVAPID(w WebPushConfig) error {
	if w.VAPIDPublicKey == "" || w.VAPIDPrivateKey == "" {
		return fmt.Errorf("alerts.webpush.vapid_public_key and vapid_private_key must both be set")
	}
	if b, err := b64url(w.VAPIDPublicKey); err != nil || len(b) != 65 {
		return fmt.Errorf("alerts.webpush.vapid_public_key must be base64url of a 65-byte P-256 point")
	}
	if b, err := b64url(w.VAPIDPrivateKey); err != nil || len(b) != 32 {
		return fmt.Errorf("alerts.webpush.vapid_private_key must be base64url of a 32-byte P-256 scalar")
	}
	if !strings.HasPrefix(w.Subject, "mailto:") && !strings.HasPrefix(w.Subject, "https://") {
		return fmt.Errorf("alerts.webpush.subject must be a mailto: or https:// contact")
	}
	return nil
}

func validatePushSubscription(s PushSubscription) error {
	if s.Username == "" {
		return fmt.Errorf("username is required")
	}
	if u, err := url.Parse(s.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("endpoint must be an https:// URL")
	}
	if b, err := b64url(s.P256DH); err != nil || len(b) != 65 {
		return fmt.Errorf("p256dh must be base64url of a 65-byte P-256 point")
	}
	if b, err := b64url(s.Auth); err != nil || len(b) != 16 {
		return fmt.Errorf("auth must be base64url of 16 bytes")
	}
	return nil
}

func b64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// WithPushSubscription returns a validated copy of c with s added, replacing any existing entry
// for the same endpoint (re-subscribing the same browser, possibly under another user).
func (c *Config) WithPushSubscription(s PushSubscription) (*Config, error) {
	next := c.clone()
	out := next.PushSubscriptions[:0]
	for _, e := range next.PushSubscriptions {
		if e.Endpoint != s.Endpoint {
			out = append(out, e)
		}
	}
	next.PushSubscriptions = append(out, s)
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithoutPushSubscription returns a copy of c without the subscription for endpoint, and whether
// one was removed.
func (c *Config) WithoutPushSubscription(endpoint string) (*Config, bool) {
	next := c.clone()
	out := next.PushSubscriptions[:0]
	for _, e := range next.PushSubscriptions {
		if e.Endpoint != endpoint {
			out = append(out, e)
		}
	}
	removed := len(out) != len(c.PushSubscriptions)
	next.PushSubscriptions = out
	return next, removed
}

// PushSubscriptionsFor lists the subscriptions belonging to username (case-insensitive).
func (c *Config) PushSubscriptionsFor(username string) []PushSubscription {
	var out []PushSubscription
	for _, s := range c.PushSubscriptions {
		if strings.EqualFold(s.Username, username) {
			out = append(out, s)
		}
	}
	return out
}

// ── Locations ────────────────────────────────────────────────────────────────

// Location is an office/building. Each room belongs to one (Room.Location, else
// Config.DefaultLocation), and its alerts go where Delivery says (that location's group, its person
// as a Lark DM, or both) and to browser-push subscribers who chose that location.
type Location struct {
	ID            string `yaml:"id"`                        // stable key, e.g. the Workspace building ID
	Name          string `yaml:"name"`                      // shown in /admin and in alert text
	LarkUserEmail string `yaml:"lark_user_email,omitempty"` // the person DM'd for this location's alerts
	// LarkChatID (oc_…) is this location's own group, used instead of the global
	// alerts.lark_fallback_chat_id when the person can't be reached. Optional.
	LarkChatID string `yaml:"lark_chat_id,omitempty"`
	// Delivery picks where this location's Lark alerts go: "dm", "group" or "both". Empty keeps
	// the original behaviour (DM the person, falling back to the group only if that fails).
	Delivery string `yaml:"delivery,omitempty"`
}

// Lark delivery modes for Location.Delivery.
const (
	DeliveryDM    = "dm"    // the person only; the groups are used only if the DM fails
	DeliveryGroup = "group" // the location's group only; the global fallback only if that fails
	DeliveryBoth  = "both"  // the person and the group; the global fallback only if both fail
)

// LarkAppConfig is the Lark custom app the broker sends as. AppID is not secret; AppSecret is
// ("${LARK_APP_SECRET}"). BaseURL defaults to the Larksuite (global) Open API.
type LarkAppConfig struct {
	AppID     string `yaml:"app_id,omitempty"`
	AppSecret string `yaml:"app_secret,omitempty"`
	BaseURL   string `yaml:"base_url,omitempty"`
}

// DefaultLarkBaseURL is Larksuite's Open API. Feishu (mainland China) tenants use
// https://open.feishu.cn/open-apis instead.
const DefaultLarkBaseURL = "https://open.larksuite.com/open-apis"

// LocationByID finds a location by ID.
func (c *Config) LocationByID(id string) (Location, bool) {
	for _, l := range c.Locations {
		if l.ID == id {
			return l, true
		}
	}
	return Location{}, false
}

// LocationOf is the location a room's alerts route to: its own, else the default ("" when no
// locations are configured at all).
func (c *Config) LocationOf(room Room) string {
	if room.Location != "" {
		return room.Location
	}
	return c.DefaultLocation
}

func (c *Config) validateLocations() error {
	seen := map[string]bool{}
	for i, l := range c.Locations {
		switch {
		case l.ID == "":
			return fmt.Errorf("locations[%d]: id is required", i)
		case seen[l.ID]:
			return fmt.Errorf("locations: duplicate id %q", l.ID)
		case strings.Contains(l.ID, ","):
			return fmt.Errorf("location id %q can't contain a comma", l.ID)
		case strings.TrimSpace(l.Name) == "":
			return fmt.Errorf("location %q: name is required", l.ID)
		case l.LarkUserEmail != "" && !validEmail(l.LarkUserEmail):
			return fmt.Errorf("location %q: lark_user_email %q is not an email address", l.ID, l.LarkUserEmail)
		case l.LarkChatID != "" && !strings.HasPrefix(l.LarkChatID, "oc_"):
			return fmt.Errorf("location %q: lark_chat_id must be a Lark chat ID (oc_…)", l.ID)
		case l.Delivery != "" && l.Delivery != DeliveryDM && l.Delivery != DeliveryGroup && l.Delivery != DeliveryBoth:
			return fmt.Errorf("location %q: delivery %q must be dm, group or both", l.ID, l.Delivery)
		case (l.Delivery == DeliveryDM || l.Delivery == DeliveryBoth) && l.LarkUserEmail == "":
			return fmt.Errorf("location %q: delivery %q needs lark_user_email", l.ID, l.Delivery)
		case (l.Delivery == DeliveryGroup || l.Delivery == DeliveryBoth) && l.LarkChatID == "":
			return fmt.Errorf("location %q: delivery %q needs lark_chat_id", l.ID, l.Delivery)
		}
		seen[l.ID] = true
	}
	for _, u := range c.Users {
		if u.Role == "admin" && len(u.Locations) > 0 {
			return fmt.Errorf("user %q: admins always see every location, so they can't be limited to some", u.Username)
		}
		listed := map[string]bool{}
		for _, id := range u.Locations {
			switch {
			case !seen[id]:
				return fmt.Errorf("user %q: unknown location %q", u.Username, id)
			case listed[id]:
				return fmt.Errorf("user %q: location %q is listed twice", u.Username, id)
			}
			listed[id] = true
		}
	}
	if len(c.Locations) > 0 && c.DefaultLocation == "" {
		return fmt.Errorf("default_location is required when locations are configured (rooms without a location use it)")
	}
	if c.DefaultLocation != "" && !seen[c.DefaultLocation] {
		return fmt.Errorf("default_location %q is not one of locations", c.DefaultLocation)
	}
	for _, r := range c.Rooms {
		if r.Location != "" && !seen[r.Location] {
			return fmt.Errorf("room %q: unknown location %q", r.DeviceID, r.Location)
		}
	}
	return nil
}

// validateLarkApp: the app credentials must be present, and every alert must have somewhere to
// go — each location needs a person or a chat, unless the global fallback chat is set.
func (c *Config) validateLarkApp() error {
	a := c.Alerts
	if a.LarkApp.AppID == "" {
		return fmt.Errorf("alerts.lark_app.app_id is empty")
	}
	if a.LarkApp.AppSecret == "" {
		return fmt.Errorf("alerts.lark_app.app_secret is empty — set its secret (${LARK_APP_SECRET})")
	}
	if a.LarkApp.BaseURL != "" {
		if err := requireHTTPS("alerts.lark_app.base_url", a.LarkApp.BaseURL); err != nil {
			return err
		}
	}
	if a.LarkFallbackChatID != "" && !strings.HasPrefix(a.LarkFallbackChatID, "oc_") {
		return fmt.Errorf("alerts.lark_fallback_chat_id must be a Lark chat ID (oc_…)")
	}
	if a.LarkFallbackChatID != "" {
		return nil
	}
	if len(c.Locations) == 0 {
		return fmt.Errorf("no locations and no alerts.lark_fallback_chat_id: Lark alerts would have nowhere to go")
	}
	for _, l := range c.Locations {
		if l.LarkUserEmail == "" && l.LarkChatID == "" {
			return fmt.Errorf("location %q has no lark_user_email or lark_chat_id and alerts.lark_fallback_chat_id is unset", l.ID)
		}
	}
	return nil
}

func validEmail(s string) bool {
	at := strings.LastIndex(s, "@")
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t<>") && strings.Contains(s[at:], ".")
}

// WithLocation returns a validated copy of c with l added or, if its ID exists, replaced. The
// first location ever added also becomes the default, so a fresh install has a valid config.
func (c *Config) WithLocation(l Location) (*Config, error) {
	next := c.clone()
	replaced := false
	for i := range next.Locations {
		if next.Locations[i].ID == l.ID {
			next.Locations[i] = l
			replaced = true
		}
	}
	if !replaced {
		next.Locations = append(next.Locations, l)
	}
	if next.DefaultLocation == "" {
		next.DefaultLocation = l.ID
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithoutLocation removes a location. Refused while any room or subscription still points at it,
// or while it's the default — those must be moved first, so no alert silently loses its route.
func (c *Config) WithoutLocation(id string) (*Config, error) {
	if c.DefaultLocation == id && len(c.Locations) > 1 {
		return nil, fmt.Errorf("location %q is the default; make another location the default first", id)
	}
	for _, r := range c.Rooms {
		if r.Location == id {
			return nil, fmt.Errorf("room %q is in location %q; move it first", r.DeviceID, id)
		}
	}
	// An emptied list means "sees everything", so a location still on someone's list must be
	// taken off it first rather than silently widening that account's access.
	for _, u := range c.Users {
		if u.CanSeeLocation(id) && u.Limited() {
			return nil, fmt.Errorf("user %q is limited to location %q; edit their access first", u.Username, id)
		}
	}
	next := c.clone()
	out := next.Locations[:0]
	for _, l := range next.Locations {
		if l.ID != id {
			out = append(out, l)
		}
	}
	next.Locations = out
	if next.DefaultLocation == id {
		next.DefaultLocation = ""
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// WithDefaultLocation sets the location used by rooms that don't name one.
func (c *Config) WithDefaultLocation(id string) (*Config, error) {
	next := c.clone()
	next.DefaultLocation = id
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}
