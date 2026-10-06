package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// A path that stands in for a real webhook's secret part; it must never appear in a page,
// redirect, or error.
const fakeHookSecret = "hook/SECRETPART-0123456789abcdef"

func adminPost(t *testing.T, h http.Handler, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	tok := s.sessions.Create(auth.RoleAdmin, testAdminUsername, auth.SessionFlags{})
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: adminUI.cookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func adminGet(t *testing.T, h http.Handler, s *Server, path string) string {
	t.Helper()
	tok := s.sessions.Create(auth.RoleAdmin, testAdminUsername, auth.SessionFlags{})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: adminUI.cookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

func withSecrets(t *testing.T, s *Server, larkURL string) {
	t.Helper()
	c := *s.cfg.Load()
	c.Alerts.LarkWebhookURL = larkURL
	c.Alerts.WebPush = vapidConfig(t)
	c.Alerts.WebhookURL = "https://hooks.slack.example/services/" + fakeHookSecret
	s.cfg.Store(&c)
}

// Regression: saving the Alerts thresholds must not wipe channels or channel secrets.
func TestAdminAlertsSaveKeepsChannelsAndSecrets(t *testing.T) {
	s := testServerWithAuth(t)
	withSecrets(t, s, "https://open.larksuite.com/open-apis/bot/v2/"+fakeHookSecret)
	c := *s.cfg.Load()
	c.Alerts.Channels = []string{"lark", "webpush"}
	s.cfg.Store(&c)

	rec := adminPost(t, s.Handler(), s, "/admin/alerts/save", url.Values{
		"low_battery_pct": {"40"}, "clear_pct": {"55"}, "min_renotify": {"24h"}, "stale_after": {"1h"}})
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "error") {
		t.Fatalf("save failed: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	got := s.cfg.Load().Alerts
	if got.LowBatteryPct != 40 {
		t.Errorf("threshold not saved: %d", got.LowBatteryPct)
	}
	if strings.Join(got.Channels, ",") != "lark,webpush" || got.LarkWebhookURL == "" || got.WebPush.VAPIDPrivateKey == "" || got.WebhookURL == "" {
		t.Errorf("alerts save wiped notification config: %+v", got.Channels)
	}
}

// The admin page shows each channel's secret as set/unset only — never a value.
func TestAdminPageNeverRendersChannelSecrets(t *testing.T) {
	s := testServerWithAuth(t)
	withSecrets(t, s, "https://open.larksuite.com/open-apis/bot/v2/"+fakeHookSecret)
	body := adminGet(t, s.Handler(), s, "/admin")
	if !strings.Contains(body, `id="notifications"`) {
		t.Fatal("Notifications section missing")
	}
	for name, secret := range map[string]string{
		"webhook path":      "SECRETPART",
		"vapid private key": s.cfg.Load().Alerts.WebPush.VAPIDPrivateKey,
	} {
		if strings.Contains(body, secret) {
			t.Errorf("admin page contains the %s", name)
		}
	}
	if strings.Count(body, "✅ set") != 3 {
		t.Errorf("want lark, browser push and slack all shown as set")
	}
}

func TestAdminSaveNotificationsFailsClosedAndActivates(t *testing.T) {
	s := testServerWithAuth(t)
	h := s.Handler()

	// No Lark secret: selecting it is refused and nothing changes.
	rec := adminPost(t, h, s, "/admin/notifications/save", url.Values{"channel": {"lark"}})
	if !strings.Contains(rec.Header().Get("Location"), "error=") || len(s.cfg.Load().Alerts.Channels) != 0 {
		t.Fatalf("selecting a channel without its secret must be refused: %s %v", rec.Header().Get("Location"), s.cfg.Load().Alerts.Channels)
	}

	withSecrets(t, s, "https://open.larksuite.com/open-apis/bot/v2/"+fakeHookSecret)
	rec = adminPost(t, h, s, "/admin/notifications/save", url.Values{"channel": {"webpush", "lark"}})
	if strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatalf("valid selection refused: %s", rec.Header().Get("Location"))
	}
	if got := strings.Join(s.cfg.Load().Alerts.Channels, ","); got != "lark,webpush" {
		t.Errorf("saved channels = %q", got)
	}
	if n := s.alerts.Notifier().Name(); n != "lark+webpush" || s.vapidKey() == "" {
		t.Errorf("selection saved but not activated: %s", n)
	}

	// Nothing ticked = explicit none, even though a legacy Slack webhook is set.
	adminPost(t, h, s, "/admin/notifications/save", url.Values{})
	if got := s.cfg.Load().Alerts.Channels; len(got) != 1 || got[0] != "log" {
		t.Errorf("none must save as [log], got %v", got)
	}
	if n := s.alerts.Notifier().Name(); n != "log" {
		t.Errorf("none must stop all sending (legacy Slack included), got %s", n)
	}
	if s.cfg.Load().Alerts.WebhookURL == "" {
		t.Error("turning Slack off must not delete its webhook reference")
	}
}

// A failing test send reports the failure without the webhook's secret path, both for a refused
// connection (net/http's *url.Error embeds the URL) and for a Lark rejection.
func TestAdminTestNotificationRedactsWebhook(t *testing.T) {
	s := testServerWithAuth(t)
	h := s.Handler()
	withSecrets(t, s, "https://127.0.0.1:1/open-apis/bot/v2/"+fakeHookSecret) // refused
	rec := adminPost(t, h, s, "/admin/notifications/test", url.Values{"test_channel": {"lark"}, "channel": {"webpush"}})
	loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(loc, "error=") {
		t.Fatalf("a refused connection must report failure: %s", loc)
	}
	if strings.Contains(loc, "SECRETPART") {
		t.Errorf("test-send error leaked the webhook path: %s", loc)
	}
}

func TestAdminTestNotificationSendsOneLarkMessage(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer ts.Close()
	orig := http.DefaultTransport
	http.DefaultTransport = ts.Client().Transport
	defer func() { http.DefaultTransport = orig }()

	s := testServerWithAuth(t)
	withSecrets(t, s, ts.URL+"/open-apis/bot/v2/"+fakeHookSecret)
	// The checkbox values are submitted alongside the clicked test button; test_channel must win.
	rec := adminPost(t, s.Handler(), s, "/admin/notifications/test", url.Values{"test_channel": {"lark"}, "channel": {"webpush", "lark"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "tested=lark") {
		t.Fatalf("want success redirect, got %s", loc)
	}
	if len(hits) != 1 {
		t.Errorf("want exactly one message to the (fake) Lark webhook, got %d", len(hits))
	}
}

// Browser-push test goes only to the clicking admin's own browsers.
func TestAdminTestPushOnlyReachesOwnBrowsers(t *testing.T) {
	var mu sync.Mutex
	var hit []string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = append(hit, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()
	orig := http.DefaultTransport
	http.DefaultTransport = ts.Client().Transport
	defer func() { http.DefaultTransport = orig }()

	s := testServerWithAuth(t)
	withSecrets(t, s, "https://open.larksuite.com/open-apis/bot/v2/x")
	c := s.cfg.Load()
	for _, u := range []string{testAdminUsername, testManagerUsername} {
		p, a := browserKeys(t)
		var err error
		if c, err = c.WithPushSubscription(config.PushSubscription{Username: u, Endpoint: ts.URL + "/" + u, P256DH: p, Auth: a}); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.Store(c)

	rec := adminPost(t, s.Handler(), s, "/admin/notifications/test", url.Values{"test_channel": {"webpush"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "tested=webpush") {
		t.Fatalf("want success, got %s", loc)
	}
	if len(hit) != 1 || hit[0] != "/"+testAdminUsername {
		t.Errorf("test push reached %v, want only the admin's own browser", hit)
	}
}
