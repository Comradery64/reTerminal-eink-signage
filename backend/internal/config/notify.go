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
	ChannelLark    = "lark"    // Lark custom-bot webhook — alerts.lark_webhook_url (${LARK_WEBHOOK_URL})
	ChannelWebPush = "webpush" // browser push to subscribed admins/managers — alerts.webpush.*
	ChannelSlack   = "slack"   // legacy Slack incoming webhook — alerts.webhook_url
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
			if err := requireHTTPS("alerts.lark_webhook_url", a.LarkWebhookURL); err != nil {
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
		default:
			return fmt.Errorf("alerts.channels: unknown channel %q (want lark, webpush, or slack)", ch)
		}
	}
	for i, s := range c.PushSubscriptions {
		if err := validatePushSubscription(s); err != nil {
			return fmt.Errorf("push_subscriptions[%d]: %w", i, err)
		}
	}
	return nil
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
