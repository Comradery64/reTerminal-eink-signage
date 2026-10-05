package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func vapidConfig(t *testing.T) config.WebPushConfig {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	return config.WebPushConfig{VAPIDPublicKey: b64(k.PublicKey().Bytes()), VAPIDPrivateKey: b64(k.Bytes()), Subject: "mailto:ops@example.com"}
}

func browserKeys(t *testing.T) (p256dh, authSecret string) {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	return b64(k.PublicKey().Bytes()), b64(a)
}

// Only the selected channels are built; nothing selected = log only; a mismatched VAPID pair
// refuses to build (main exits on that error).
func TestBuildNotifierGatesOnSelectedChannels(t *testing.T) {
	s := testServerWithAuth(t)
	cfg := s.cfg.Load()

	n, err := s.BuildNotifier(cfg)
	if err != nil || n != nil {
		t.Fatalf("no channels: want nil notifier (log only), got %v, %v", n, err)
	}

	lark := *cfg
	lark.Alerts.Channels = []string{"lark"}
	lark.Alerts.LarkWebhookURL = "https://open.larksuite.com/open-apis/bot/v2/hook/x"
	if n, _ := s.BuildNotifier(&lark); n == nil || n.Name() != "lark" {
		t.Fatalf("lark only: got %v", n)
	}
	if s.vapidPublicKey != "" {
		t.Error("webpush not selected, so no applicationServerKey must be advertised")
	}

	both := lark
	both.Alerts.Channels = []string{"lark", "webpush"}
	both.Alerts.WebPush = vapidConfig(t)
	if n, _ := s.BuildNotifier(&both); n == nil || n.Name() != "lark+webpush" {
		t.Fatalf("lark+webpush: got %v", n)
	}

	bad := both
	other := vapidConfig(t)
	bad.Alerts.WebPush.VAPIDPrivateKey = other.VAPIDPrivateKey
	if _, err := s.BuildNotifier(&bad); err == nil {
		t.Fatal("a VAPID private key from another pair must fail the build")
	}
}

func postJSON(t *testing.T, h http.Handler, path, cookie string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: managerUI.cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A logged-in manager enrols a browser; it's persisted against their username; only they can
// remove it; a form-encoded (cross-site-style) post is refused.
func TestPushSubscribeAndUnsubscribe(t *testing.T) {
	s := testServerWithAuth(t)
	s.vapidPublicKey = vapidConfig(t).VAPIDPublicKey
	h := s.Handler()
	mine := s.sessions.Create(managerUI.role, testManagerUsername, auth.SessionFlags{})
	p, a := browserKeys(t)
	sub := map[string]any{"endpoint": "https://push.example/abc", "keys": map[string]string{"p256dh": p, "auth": a}}

	if rec := postJSON(t, h, "/manager/api/push/subscribe", mine, sub); rec.Code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", rec.Code, rec.Body)
	}
	got := s.cfg.Load().PushSubscriptionsFor(testManagerUsername)
	if len(got) != 1 || got[0].Endpoint != "https://push.example/abc" {
		t.Fatalf("subscription not persisted for the session's user: %+v", got)
	}

	form := httptest.NewRequest(http.MethodPost, "/manager/api/push/subscribe", strings.NewReader("endpoint=https://evil.example"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.AddCookie(&http.Cookie{Name: managerUI.cookieName, Value: mine})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, form)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form post must be refused, got %d", rec.Code)
	}

	other := s.sessions.Create(auth.RoleAdmin, testAdminUsername, auth.SessionFlags{})
	postJSON(t, h, "/manager/api/push/unsubscribe", other, sub)
	if len(s.cfg.Load().PushSubscriptions) != 1 {
		t.Fatal("another user must not be able to remove someone else's subscription")
	}
	postJSON(t, h, "/manager/api/push/unsubscribe", mine, sub)
	if len(s.cfg.Load().PushSubscriptions) != 0 {
		t.Fatal("owner's unsubscribe must remove it")
	}
}

// End to end through the server's subscription store: a 410 from the push service deletes the
// subscription from the persisted config.
func TestGonePushSubscriptionIsDeletedFromConfig(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer ts.Close()
	s := testServerWithAuth(t)
	p, a := browserKeys(t)
	next, err := s.cfg.Load().WithPushSubscription(config.PushSubscription{Username: testManagerUsername, Endpoint: ts.URL + "/sub1", P256DH: p, Auth: a})
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store(next)

	vc := vapidConfig(t)
	keys, _ := notify.ParseVAPIDKeys(vc.VAPIDPublicKey, vc.VAPIDPrivateKey)
	wp := notify.NewWebPush(keys, vc.Subject, s)
	wp.Client = ts.Client()
	if err := wp.Send(context.Background(), notify.Message{Title: "x"}); err != nil {
		t.Fatalf("a 410 is cleanup, not a failure: %v", err)
	}
	if n := len(s.cfg.Load().PushSubscriptions); n != 0 {
		t.Fatalf("410'd subscription still in config (%d left)", n)
	}
}

type recordingNotifier struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (r *recordingNotifier) Name() string { return "recording" }
func (r *recordingNotifier) Send(_ context.Context, m notify.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
	return nil
}
func (r *recordingNotifier) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.msgs {
		out = append(out, m.Title)
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100 && !cond(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
}

// The stale checker uses each device's own threshold and sends offline once, then recovered.
func TestCheckStaleSendsOfflineThenRecovered(t *testing.T) {
	s := testServerWithAuth(t)
	rec := &recordingNotifier{}
	s.alerts.SetNotifier(rec)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	s.tlm.SetExpectedWake("rt-1", 600)
	s.tlm.Ingest("rt-1", telemetry.Report{BatteryPct: 80}, now.Add(-25*time.Minute)) // > 2×600s
	s.checkStale(now)
	s.checkStale(now.Add(time.Minute)) // still offline: no repeat
	waitFor(t, func() bool { return len(rec.titles()) >= 1 })

	s.tlm.Ingest("rt-1", telemetry.Report{BatteryPct: 80}, now.Add(2*time.Minute))
	s.checkStale(now.Add(3 * time.Minute))
	waitFor(t, func() bool { return len(rec.titles()) >= 2 })
	time.Sleep(50 * time.Millisecond)

	got := rec.titles()
	if len(got) != 2 || !strings.Contains(got[0], "offline: Aspen") || !strings.Contains(got[1], "back online: Aspen") {
		t.Fatalf("messages = %q, want exactly [offline, back online]", got)
	}
}

func TestNotificationsPageRendersForSignedInManager(t *testing.T) {
	s := testServerWithAuth(t)
	h := s.Handler()
	tok := s.sessions.Create(managerUI.role, testManagerUsername, auth.SessionFlags{})
	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/manager/notifications", nil)
		req.AddCookie(&http.Cookie{Name: managerUI.cookieName, Value: tok})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		return rec.Body.String()
	}
	if body := get(); !strings.Contains(body, "aren't turned on for this server") {
		t.Error("without webpush selected the page must say so, not offer a broken button")
	}
	s.vapidPublicKey = vapidConfig(t).VAPIDPublicKey
	body := get()
	if !strings.Contains(body, "Enable notifications on this browser") || !strings.Contains(body, s.vapidPublicKey) {
		t.Error("with webpush selected the page must offer enrolment with the server's public key")
	}

	req := httptest.NewRequest(http.MethodGet, "/manager/push-sw.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "showNotification") {
		t.Errorf("service worker not served: %d", rec.Code)
	}
}
