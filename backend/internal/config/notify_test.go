package config

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func notifyBase() *Config {
	c := &Config{Provider: "demo", Rooms: []Room{{DeviceID: "rt-1", Room: "r@x", TokenSHA256: strings.Repeat("a", 64)}}}
	c.Wake.Timezone = "UTC"
	return c
}

func vapidPair(t *testing.T) (pub, priv string) {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(k.Bytes())
}

// Selecting a channel without its secret is a startup error, never a silent fall-back to log-only.
func TestValidateNotifyFailsClosed(t *testing.T) {
	pub, priv := vapidPair(t)
	cases := map[string]func(c *Config){
		"lark without url": func(c *Config) { c.Alerts.Channels = []string{"lark"} },
		"lark over http": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			c.Alerts.LarkWebhookURL = "http://open.larksuite.com/hook"
		},
		"webpush without keys": func(c *Config) { c.Alerts.Channels = []string{"webpush"}; c.Alerts.WebPush.Subject = "mailto:a@b" },
		"webpush without subject": func(c *Config) {
			c.Alerts.Channels = []string{"webpush"}
			c.Alerts.WebPush = WebPushConfig{VAPIDPublicKey: pub, VAPIDPrivateKey: priv}
		},
		"slack without url": func(c *Config) { c.Alerts.Channels = []string{"slack"} },
		"unknown channel":   func(c *Config) { c.Alerts.Channels = []string{"email"} },
		"duplicate channel": func(c *Config) {
			c.Alerts.Channels = []string{"lark", "lark"}
			c.Alerts.LarkWebhookURL = "https://open.larksuite.com/hook"
		},
		"bad subscription": func(c *Config) {
			c.PushSubscriptions = []PushSubscription{{Username: "a", Endpoint: "https://push.example/x", P256DH: "short", Auth: "short"}}
		},
	}
	for name, mutate := range cases {
		c := notifyBase()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}

	ok := notifyBase()
	ok.Alerts.Channels = []string{"lark", "webpush"}
	ok.Alerts.LarkWebhookURL = "https://open.larksuite.com/open-apis/bot/v2/hook/x"
	ok.Alerts.WebPush = WebPushConfig{VAPIDPublicKey: pub, VAPIDPrivateKey: priv, Subject: "mailto:ops@example.com"}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid lark+webpush config rejected: %v", err)
	}
}

// The secret arrives via ${LARK_WEBHOOK_URL}; if that variable is unset the broker must not start.
func TestLoadRefusesUnsetLarkSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := "provider: demo\nwake: {timezone: UTC}\nrooms:\n  - {device_id: rt-1, room: r@x, token_sha256: " + strings.Repeat("a", 64) + "}\n" +
		"alerts:\n  channels: [lark]\n  lark_webhook_url: \"${MD_TEST_UNSET_LARK_URL}\"\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("MD_TEST_UNSET_LARK_URL")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "MD_TEST_UNSET_LARK_URL") {
		t.Fatalf("Load must refuse an unset channel secret and name it; got %v", err)
	}
}

// Secrets loaded from ${VAR} must be written back as the ${VAR} token, never the value.
func TestNotifySecretsPersistAsEnvRefs(t *testing.T) {
	pub, priv := vapidPair(t)
	t.Setenv("MD_TEST_LARK_URL", "https://open.larksuite.com/open-apis/bot/v2/hook/0123456789abcdef")
	t.Setenv("MD_TEST_VAPID_PRIV", priv)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := "provider: demo\nwake: {timezone: UTC}\nrooms:\n  - {device_id: rt-1, room: r@x, token_sha256: " + strings.Repeat("a", 64) + "}\n" +
		"alerts:\n  channels: [lark, webpush]\n  lark_webhook_url: \"${MD_TEST_LARK_URL}\"\n" +
		"  webpush: {vapid_public_key: " + pub + ", vapid_private_key: \"${MD_TEST_VAPID_PRIV}\", subject: \"mailto:ops@example.com\"}\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.MarshalForPersist()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), priv) || strings.Contains(string(out), "0123456789abcdef") {
		t.Error("a secret value was written back in plaintext")
	}
	if !strings.Contains(string(out), "${MD_TEST_LARK_URL}") || !strings.Contains(string(out), "${MD_TEST_VAPID_PRIV}") {
		t.Error("the ${VAR} references were not restored on persist")
	}
}

func TestEffectiveChannelsLegacySlack(t *testing.T) {
	a := AlertConfig{WebhookURL: "https://hooks.slack.com/x"}
	if got := a.EffectiveChannels(); len(got) != 1 || got[0] != "slack" {
		t.Errorf("legacy webhook_url config must keep notifying Slack, got %v", got)
	}
	if got := (AlertConfig{}).EffectiveChannels(); len(got) != 0 {
		t.Errorf("no channels and no webhook = log only, got %v", got)
	}
}

func TestLogChannelMustStandAlone(t *testing.T) {
	c := notifyBase()
	c.Alerts.Channels = []string{"log"}
	if err := c.Validate(); err != nil {
		t.Errorf("[log] alone is valid: %v", err)
	}
	c.Alerts.Channels = []string{"log", "slack"}
	c.Alerts.WebhookURL = "https://hooks.slack.com/x"
	if err := c.Validate(); err == nil {
		t.Error("log combined with a real channel is contradictory and must be rejected")
	}
}
