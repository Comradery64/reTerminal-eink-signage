package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/configstore"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

// Production-shaped: secrets arrive as ${VAR}, an admin/subscription change persists the config,
// and both the live in-memory config and a fresh Load of what was persisted must still hold the
// resolved secrets — while the persisted bytes hold only the ${VAR} references.
func TestConfigSaveKeepsResolvedSecrets(t *testing.T) {
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	b64 := base64.RawURLEncoding.EncodeToString
	secrets := map[string]string{
		"LARK_APP_SECRET":           "Lk" + strings.Repeat("x", 30), // 32 chars, like prod
		"WEBPUSH_VAPID_PUBLIC_KEY":  b64(k.PublicKey().Bytes()),     // 87
		"WEBPUSH_VAPID_PRIVATE_KEY": b64(k.Bytes()),                 // 43
		"SESSION_SECRET":            strings.Repeat("s", 40),
	}
	for name, v := range secrets {
		t.Setenv(name, v)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yml := `provider: demo
wake: {timezone: UTC}
auth: {session_secret: "${SESSION_SECRET}"}
users:
  - {username: admin, password_sha256: "` + strings.Repeat("a", 64) + `", role: admin}
rooms:
  - {device_id: rt-1, room: r@x, token_sha256: ` + strings.Repeat("a", 64) + `}
locations:
  - {id: loc-hq, name: HQ, lark_user_email: hq@example.com}
default_location: loc-hq
alerts:
  channels: [lark, webpush]
  lark_fallback_chat_id: oc_fallback
  lark_app: {app_id: cli_x, app_secret: "${LARK_APP_SECRET}"}
  webpush: {vapid_public_key: "${WEBPUSH_VAPID_PUBLIC_KEY}", vapid_private_key: "${WEBPUSH_VAPID_PRIVATE_KEY}", subject: "mailto:ops@example.com"}
config_persistence: {mode: file}
`
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := configstore.New(configstore.Options{Mode: "file", ConfigPath: path, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	s := New(config.NewLive(cfg), cache.New(), telemetry.New(), notify.NewManagerWith(nil, 45, 55, 24*time.Hour, log), store, log)

	check := func(stage string, c *config.Config) {
		t.Helper()
		if c.Alerts.LarkApp.AppSecret != secrets["LARK_APP_SECRET"] ||
			c.Alerts.WebPush.VAPIDPrivateKey != secrets["WEBPUSH_VAPID_PRIVATE_KEY"] ||
			c.Alerts.WebPush.VAPIDPublicKey != secrets["WEBPUSH_VAPID_PUBLIC_KEY"] {
			t.Errorf("%s: resolved secrets lost (lark set=%v, vapid priv set=%v, vapid pub set=%v)", stage,
				c.Alerts.LarkApp.AppSecret != "", c.Alerts.WebPush.VAPIDPrivateKey != "", c.Alerts.WebPush.VAPIDPublicKey != "")
		}
	}

	// subscribe, then the push service's 410 removes it — the 02:09 sequence
	pk, _ := ecdh.P256().GenerateKey(rand.Reader)
	next, err := s.cfg.Load().WithPushSubscription(config.PushSubscription{Username: "admin", Endpoint: "https://push.example/x",
		P256DH: b64(pk.PublicKey().Bytes()), Auth: b64(make([]byte, 16))})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyConfig(t.Context(), next, false); err != nil {
		t.Fatal(err)
	}
	check("in memory after subscribe", s.cfg.Load())
	s.RemovePushSubscription("https://push.example/x")
	check("in memory after removal", s.cfg.Load())

	// Every other /admin save path goes through applyConfig too; each must leave the secrets alone.
	h := s.Handler()
	for _, step := range []struct {
		path string
		form url.Values
	}{
		{"/admin/alerts/save", url.Values{"low_battery_pct": {"40"}, "clear_pct": {"55"}, "min_renotify": {"24h"}, "stale_after": {"1h"}}},
		{"/admin/rooms/save", url.Values{"original_device_id": {"rt-1"}, "device_id": {"rt-1"}, "name": {"Room 1"}, "room": {"r@x"}, "location": {"loc-hq"}}},
		{"/admin/locations/save", url.Values{"id": {"loc-sf"}, "name": {"SF"}, "lark_user_email": {"sf@example.com"}}},
		{"/admin/locations/default", url.Values{"id": {"loc-sf"}}},
		{"/admin/access/save", url.Values{"username": {"viewer1"}, "role": {"viewer"}, "password": {"a-long-enough-password-1"}}},
		{"/admin/notifications/save", url.Values{"channel": {"lark", "webpush"}}},
	} {
		rec := adminPost(t, h, s, step.path, step.form)
		if loc := rec.Header().Get("Location"); strings.Contains(loc, "error=") {
			t.Fatalf("%s failed: %s", step.path, loc)
		}
		check("in memory after "+step.path, s.cfg.Load())
	}

	raw, _ := os.ReadFile(path)
	for name, v := range secrets {
		if strings.Contains(string(raw), v) {
			t.Errorf("persisted config contains the %s value in plaintext", name)
		}
		if !strings.Contains(string(raw), "${"+name+"}") {
			t.Errorf("persisted config lost the ${%s} reference", name)
		}
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("persisted config no longer loads: %v", err)
	}
	check("after restart (reload of persisted file)", reloaded)
}
