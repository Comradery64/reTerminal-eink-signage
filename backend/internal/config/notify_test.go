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

func larkOK(c *Config) {
	c.Alerts.LarkApp = LarkAppConfig{AppID: "cli_x", AppSecret: "s"}
	c.Alerts.LarkFallbackChatID = "oc_fallback"
}

// Selecting a channel without what it needs is a startup error, never a silent fall-back.
func TestValidateNotifyFailsClosed(t *testing.T) {
	pub, priv := vapidPair(t)
	cases := map[string]func(c *Config){
		"lark without app secret": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			larkOK(c)
			c.Alerts.LarkApp.AppSecret = ""
		},
		"lark without app id": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			larkOK(c)
			c.Alerts.LarkApp.AppID = ""
		},
		"lark with nowhere to send": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			c.Alerts.LarkApp = LarkAppConfig{AppID: "cli_x", AppSecret: "s"}
		},
		"lark location with no person, no chat, no fallback": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			c.Alerts.LarkApp = LarkAppConfig{AppID: "cli_x", AppSecret: "s"}
			c.Locations = []Location{{ID: "a", Name: "A", LarkUserEmail: "a@example.com"}, {ID: "b", Name: "B"}}
			c.DefaultLocation = "a"
		},
		"delivery not a mode": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			larkOK(c)
			c.Locations = []Location{{ID: "a", Name: "A", LarkUserEmail: "a@example.com", LarkChatID: "oc_a", Delivery: "dms"}}
			c.DefaultLocation = "a"
		},
		"delivery dm without a person": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			larkOK(c)
			c.Locations = []Location{{ID: "a", Name: "A", LarkChatID: "oc_a", Delivery: "dm"}}
			c.DefaultLocation = "a"
		},
		"delivery group without a group": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			larkOK(c)
			c.Locations = []Location{{ID: "a", Name: "A", LarkUserEmail: "a@example.com", Delivery: "group"}}
			c.DefaultLocation = "a"
		},
		"delivery both with only a group": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			larkOK(c)
			c.Locations = []Location{{ID: "a", Name: "A", LarkChatID: "oc_a", Delivery: "both"}}
			c.DefaultLocation = "a"
		},
		"unknown alert message style": func(c *Config) { c.Alerts.MessageStyle = "shouting" },
		"lark fallback not a chat id": func(c *Config) {
			c.Alerts.Channels = []string{"lark"}
			larkOK(c)
			c.Alerts.LarkFallbackChatID = "general"
		},
		"webpush without keys": func(c *Config) { c.Alerts.Channels = []string{"webpush"}; c.Alerts.WebPush.Subject = "mailto:a@b" },
		"webpush without subject": func(c *Config) {
			c.Alerts.Channels = []string{"webpush"}
			c.Alerts.WebPush = WebPushConfig{VAPIDPublicKey: pub, VAPIDPrivateKey: priv}
		},
		"slack without url": func(c *Config) { c.Alerts.Channels = []string{"slack"} },
		"unknown channel":   func(c *Config) { c.Alerts.Channels = []string{"email"} },
		"duplicate channel": func(c *Config) { c.Alerts.Channels = []string{"lark", "lark"}; larkOK(c) },
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
	larkOK(ok)
	ok.Alerts.WebPush = WebPushConfig{VAPIDPublicKey: pub, VAPIDPrivateKey: priv, Subject: "mailto:ops@example.com"}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid lark+webpush config rejected: %v", err)
	}

	// Every delivery mode is accepted when its fields are present, and an unset one still is.
	for _, mode := range []string{"", DeliveryDM, DeliveryGroup, DeliveryBoth} {
		c := notifyBase()
		c.Alerts.Channels = []string{"lark"}
		larkOK(c)
		c.Locations = []Location{{ID: "a", Name: "A", LarkUserEmail: "a@example.com", LarkChatID: "oc_a", Delivery: mode}}
		c.DefaultLocation = "a"
		if err := c.Validate(); err != nil {
			t.Errorf("delivery %q rejected: %v", mode, err)
		}
	}
}

func TestValidateLocations(t *testing.T) {
	two := func(c *Config) {
		c.Locations = []Location{{ID: "hq", Name: "HQ", LarkUserEmail: "hq@example.com"}, {ID: "sf", Name: "SF"}}
		c.DefaultLocation = "hq"
	}
	bad := map[string]func(c *Config){
		"room in unknown location":    func(c *Config) { two(c); c.Rooms[0].Location = "nyc" },
		"room location, no locations": func(c *Config) { c.Rooms[0].Location = "hq" },
		"duplicate id":                func(c *Config) { two(c); c.Locations[1].ID = "hq" },
		"missing name":                func(c *Config) { two(c); c.Locations[1].Name = " " },
		"bad email":                   func(c *Config) { two(c); c.Locations[1].LarkUserEmail = "not-an-email" },
		"bad chat id":                 func(c *Config) { two(c); c.Locations[1].LarkChatID = "general" },
		"no default":                  func(c *Config) { two(c); c.DefaultLocation = "" },
		"unknown default":             func(c *Config) { two(c); c.DefaultLocation = "nyc" },
		"subscription unknown location": func(c *Config) {
			two(c)
			k, _ := ecdh.P256().GenerateKey(rand.Reader)
			c.PushSubscriptions = []PushSubscription{{Username: "u", Location: "nyc", Endpoint: "https://push.example/x",
				P256DH: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}}
		},
	}
	for name, mutate := range bad {
		c := notifyBase()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	c := notifyBase()
	two(c)
	c.Rooms[0].Location = "sf"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid locations rejected: %v", err)
	}
	if got := c.LocationOf(Room{}); got != "hq" {
		t.Errorf("room without a location must use the default, got %q", got)
	}
	if got := c.LocationOf(c.Rooms[0]); got != "sf" {
		t.Errorf("room's own location must win, got %q", got)
	}
}

func TestWithoutLocationRefusesWhileInUse(t *testing.T) {
	c := notifyBase()
	c.Locations = []Location{{ID: "hq", Name: "HQ"}, {ID: "sf", Name: "SF"}}
	c.DefaultLocation = "hq"
	c.Rooms[0].Location = "sf"
	if _, err := c.WithoutLocation("sf"); err == nil {
		t.Error("removing a location a room is in must be refused")
	}
	if _, err := c.WithoutLocation("hq"); err == nil {
		t.Error("removing the default location must be refused while others exist")
	}
	c.Rooms[0].Location = ""
	next, err := c.WithoutLocation("sf")
	if err != nil || len(next.Locations) != 1 {
		t.Errorf("removing an unused location: %v", err)
	}
}

// The app secret arrives via ${LARK_APP_SECRET}; if that variable is unset the broker must not start.
func TestLoadRefusesUnsetLarkAppSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := "provider: demo\nwake: {timezone: UTC}\nrooms:\n  - {device_id: rt-1, room: r@x, token_sha256: " + strings.Repeat("a", 64) + "}\n" +
		"alerts:\n  channels: [lark]\n  lark_fallback_chat_id: oc_x\n  lark_app: {app_id: cli_x, app_secret: \"${MD_TEST_UNSET_LARK_SECRET}\"}\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("MD_TEST_UNSET_LARK_SECRET")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "MD_TEST_UNSET_LARK_SECRET") {
		t.Fatalf("Load must refuse an unset app secret and name it; got %v", err)
	}
}

// Secrets loaded from ${VAR} must be written back as the ${VAR} token, never the value.
func TestNotifySecretsPersistAsEnvRefs(t *testing.T) {
	pub, priv := vapidPair(t)
	t.Setenv("MD_TEST_LARK_SECRET", "lark-app-secret-0123456789abcdef")
	t.Setenv("MD_TEST_VAPID_PRIV", priv)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := "provider: demo\nwake: {timezone: UTC}\nrooms:\n  - {device_id: rt-1, room: r@x, token_sha256: " + strings.Repeat("a", 64) + "}\n" +
		"alerts:\n  channels: [lark, webpush]\n  lark_fallback_chat_id: oc_x\n  lark_app: {app_id: cli_x, app_secret: \"${MD_TEST_LARK_SECRET}\"}\n" +
		"  webpush: {vapid_public_key: " + pub + ", vapid_private_key: \"${MD_TEST_VAPID_PRIV}\", subject: \"mailto:ops@example.com\"}\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerts.LarkApp.BaseURL != DefaultLarkBaseURL {
		t.Errorf("base_url default = %q", c.Alerts.LarkApp.BaseURL)
	}
	out, err := c.MarshalForPersist()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), priv) || strings.Contains(string(out), "0123456789abcdef") {
		t.Error("a secret value was written back in plaintext")
	}
	if !strings.Contains(string(out), "${MD_TEST_LARK_SECRET}") || !strings.Contains(string(out), "${MD_TEST_VAPID_PRIV}") {
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
