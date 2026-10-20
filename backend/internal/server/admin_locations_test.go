package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// fakeLarkServer records who was messaged (receive_id_type + receive_id). Local only.
type fakeLarkServer struct {
	mu   sync.Mutex
	sent []string
	ts   *httptest.Server
}

func newFakeLarkServer(t *testing.T) *fakeLarkServer {
	f := &fakeLarkServer{}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal") {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-x","expire":7200}`))
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sent = append(f.sent, r.URL.Query().Get("receive_id_type")+":"+body["receive_id"])
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	orig := http.DefaultTransport
	http.DefaultTransport = f.ts.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = orig; f.ts.Close() })
	return f
}

func (f *fakeLarkServer) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func withLocations(t *testing.T, s *Server, larkBase string) {
	t.Helper()
	c := *s.cfg.Load()
	c.Locations = []config.Location{
		{ID: "loc-hq", Name: "HQ", LarkUserEmail: "hq-person@example.com"},
		{ID: "loc-sf", Name: "SF", LarkUserEmail: "sf-person@example.com"},
	}
	c.DefaultLocation = "loc-hq"
	c.Alerts.LarkApp = config.LarkAppConfig{AppID: "cli_test", AppSecret: "s", BaseURL: larkBase}
	c.Alerts.LarkFallbackChatID = "oc_fallback"
	c.Alerts.Channels = []string{"lark"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s.cfg.Store(&c)
}

// An alert for a room DMs that room's location's person — and nobody else.
func TestAlertRoutesToRoomsLocationPerson(t *testing.T) {
	f := newFakeLarkServer(t)
	s := testServerWithAuth(t)
	withLocations(t, s, f.ts.URL+"/open-apis")
	c := *s.cfg.Load()
	c.Rooms = append([]config.Room(nil), c.Rooms...)
	c.Rooms[0].Location = "loc-sf"
	s.cfg.Store(&c)
	if err := s.ActivateChannels(s.cfg.Load()); err != nil {
		t.Fatal(err)
	}

	s.alerts.EvaluateBattery("rt-1", "Aspen", 40, 3600, time.Now())
	waitFor(t, func() bool { return len(f.got()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); len(got) != 1 || got[0] != "email:sf-person@example.com" {
		t.Fatalf("sent = %v, want exactly one DM to the SF person", got)
	}

	// A room with no location of its own uses the default (HQ).
	c2 := *s.cfg.Load()
	c2.Rooms = append([]config.Room(nil), c2.Rooms...)
	c2.Rooms[0].Location = ""
	s.cfg.Store(&c2)
	s.alerts.EvaluateStale("rt-1", "Aspen", true, time.Hour, time.Now())
	waitFor(t, func() bool { return len(f.got()) >= 2 })
	if got := f.got(); got[1] != "email:hq-person@example.com" {
		t.Fatalf("default-location alert went to %v", got[1])
	}
}

// Browser push reaches only subscribers of the alert's location, plus "every location" ones.
func TestPushSubscriptionsFilterByLocation(t *testing.T) {
	s := testServerWithAuth(t)
	withLocations(t, s, config.DefaultLarkBaseURL)
	c := s.cfg.Load()
	for _, sub := range []struct{ user, loc string }{{"a", "loc-hq"}, {"b", "loc-sf"}, {"c", ""}} {
		p, a := browserKeys(t)
		var err error
		if c, err = c.WithPushSubscription(config.PushSubscription{Username: sub.user, Location: sub.loc, Endpoint: "https://push.example/" + sub.user, P256DH: p, Auth: a}); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.Store(c)
	got := map[string]bool{}
	for _, sub := range s.PushSubscriptions("loc-sf") {
		got[sub.Endpoint] = true
	}
	if len(got) != 2 || !got["https://push.example/b"] || !got["https://push.example/c"] {
		t.Fatalf("loc-sf subscribers = %v, want the SF one and the every-location one", got)
	}
}

func TestPushSubscribeRecordsLocation(t *testing.T) {
	s := testServerWithAuth(t)
	withLocations(t, s, config.DefaultLarkBaseURL)
	s.vapidPublicKey = vapidConfig(t).VAPIDPublicKey
	h := s.Handler()
	tok := s.sessions.Create(managerUI.role, testManagerUsername, auth.SessionFlags{})
	p, a := browserKeys(t)
	sub := map[string]any{"endpoint": "https://push.example/m", "location": "loc-sf", "keys": map[string]string{"p256dh": p, "auth": a}}
	if rec := postJSON(t, h, "/manager/api/push/subscribe", tok, sub); rec.Code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", rec.Code, rec.Body)
	}
	if got := s.cfg.Load().PushSubscriptions; len(got) != 1 || got[0].Location != "loc-sf" {
		t.Fatalf("location not recorded: %+v", got)
	}
	sub["location"] = "nowhere"
	sub["endpoint"] = "https://push.example/n"
	if rec := postJSON(t, h, "/manager/api/push/subscribe", tok, sub); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown location must be refused, got %d", rec.Code)
	}
}

func TestAdminLocationsSaveDefaultAndGuardedDelete(t *testing.T) {
	s := testServerWithAuth(t)
	h := s.Handler()
	rec := adminPost(t, h, s, "/admin/locations/save", url.Values{"id": {"loc-hq"}, "name": {"HQ"}, "lark_user_email": {"hq-person@example.com"}})
	if strings.Contains(rec.Header().Get("Location"), "error") {
		t.Fatalf("save failed: %s", rec.Header().Get("Location"))
	}
	if s.cfg.Load().DefaultLocation != "loc-hq" {
		t.Error("the first location must become the default")
	}
	adminPost(t, h, s, "/admin/locations/save", url.Values{"id": {"loc-sf"}, "name": {"SF"}})
	rec = adminPost(t, h, s, "/admin/locations/save", url.Values{"id": {"loc-x"}, "name": {"X"}, "lark_user_email": {"nope"}})
	if !strings.Contains(rec.Header().Get("Location"), "error") {
		t.Error("an invalid email must be refused")
	}

	// Put the room in SF via the room form, then try to delete SF.
	adminPost(t, h, s, "/admin/rooms/save", url.Values{"original_device_id": {"rt-1"}, "device_id": {"rt-1"}, "name": {"Aspen"}, "room": {"a@x"}, "location": {"loc-sf"}})
	if r, _ := s.cfg.Load().RoomByDeviceID("rt-1"); r.Location != "loc-sf" {
		t.Fatalf("room location not saved: %q", r.Location)
	}
	rec = adminPost(t, h, s, "/admin/locations/delete", url.Values{"id": {"loc-sf"}})
	if loc, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(loc, "move it first") {
		t.Errorf("deleting a location a room is in must be refused with a 'move it first' message, got %s", loc)
	}
	adminPost(t, h, s, "/admin/locations/default", url.Values{"id": {"loc-sf"}})
	if s.cfg.Load().DefaultLocation != "loc-sf" {
		t.Error("make-default did not apply")
	}
}

// The test DM goes only to the location's saved person — a recipient smuggled into the form is
// ignored — and the button asks for confirmation in the browser.
func TestAdminTestDMOnlyMessagesConfiguredPerson(t *testing.T) {
	f := newFakeLarkServer(t)
	s := testServerWithAuth(t)
	withLocations(t, s, f.ts.URL+"/open-apis")
	rec := adminPost(t, s.Handler(), s, "/admin/locations/test-dm", url.Values{"id": {"loc-sf"}, "email": {"attacker@example.com"}, "lark_user_email": {"attacker@example.com"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "dm=") {
		t.Fatalf("want success, got %s", loc)
	}
	if got := f.got(); len(got) != 1 || got[0] != "email:sf-person@example.com" {
		t.Fatalf("test DM went to %v, want only the SF person", got)
	}
	body := adminGet(t, s.Handler(), s, "/admin")
	if !strings.Contains(body, `data-confirm="Send a test Lark DM to sf-person@example.com?"`) {
		t.Error("Send test DM must ask for confirmation naming the recipient")
	}
	if strings.Contains(body, "lark-app-secret") {
		t.Error("app secret rendered")
	}
}

// withSFGroup gives SF its own group and the given delivery mode (HQ keeps the original behaviour).
func withSFGroup(t *testing.T, s *Server, mode string) {
	t.Helper()
	c := *s.cfg.Load()
	c.Locations = append([]config.Location(nil), c.Locations...)
	c.Locations[1].LarkChatID = "oc_sf_group"
	c.Locations[1].Delivery = mode
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s.cfg.Store(&c)
}

// End to end: a room in a "group" location alerts its group and never DMs the person; an
// unset location (HQ) still DMs as before.
func TestAlertHonoursLocationDelivery(t *testing.T) {
	f := newFakeLarkServer(t)
	s := testServerWithAuth(t)
	withLocations(t, s, f.ts.URL+"/open-apis")
	withSFGroup(t, s, "group")
	c := *s.cfg.Load()
	c.Rooms = append([]config.Room(nil), c.Rooms...)
	c.Rooms[0].Location = "loc-sf"
	s.cfg.Store(&c)
	if err := s.ActivateChannels(s.cfg.Load()); err != nil {
		t.Fatal(err)
	}
	s.alerts.EvaluateBattery("rt-1", "Aspen", 40, 3600, time.Now())
	waitFor(t, func() bool { return len(f.got()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); len(got) != 1 || got[0] != "chat_id:oc_sf_group" {
		t.Fatalf("sent = %v, want only the SF group (no DM)", got)
	}
}

// Delivery is saved from the form, shown, and validated: a mode that needs a field the location
// lacks is refused rather than saved to a config that would drop alerts.
func TestAdminLocationDeliverySaveAndValidation(t *testing.T) {
	s := testServerWithAuth(t)
	h := s.Handler()
	ok := url.Values{"id": {"loc-hq"}, "name": {"HQ"}, "lark_user_email": {"hq-person@example.com"}, "lark_chat_id": {"oc_hq_group"}, "delivery": {"group"}}
	if rec := adminPost(t, h, s, "/admin/locations/save", ok); strings.Contains(rec.Header().Get("Location"), "error") {
		t.Fatalf("save failed: %s", rec.Header().Get("Location"))
	}
	if l, _ := s.cfg.Load().LocationByID("loc-hq"); l.Delivery != "group" {
		t.Fatalf("delivery not saved: %q", l.Delivery)
	}
	body := adminGet(t, h, s, "/admin")
	for _, want := range []string{`name="delivery"`, `data-delivery="group"`, `action="/admin/locations/test-group"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/admin missing %s", want)
		}
	}
	for name, v := range map[string]url.Values{
		"unknown mode":      {"id": {"loc-hq"}, "name": {"HQ"}, "lark_chat_id": {"oc_hq_group"}, "delivery": {"everyone"}},
		"group, no group":   {"id": {"loc-hq"}, "name": {"HQ"}, "lark_user_email": {"hq-person@example.com"}, "delivery": {"group"}},
		"dm, no person":     {"id": {"loc-hq"}, "name": {"HQ"}, "lark_chat_id": {"oc_hq_group"}, "delivery": {"dm"}},
		"both, only person": {"id": {"loc-hq"}, "name": {"HQ"}, "lark_user_email": {"hq-person@example.com"}, "delivery": {"both"}},
	} {
		if rec := adminPost(t, h, s, "/admin/locations/save", v); !strings.Contains(rec.Header().Get("Location"), "error") {
			t.Errorf("%s: must be refused", name)
		}
	}
	if l, _ := s.cfg.Load().LocationByID("loc-hq"); l.Delivery != "group" {
		t.Errorf("a refused save changed the stored delivery to %q", l.Delivery)
	}
}

// The test message goes only to the location's saved group — a chat ID in the form is ignored —
// asks for confirmation, and a location without a group sends nothing.
func TestAdminTestGroupOnlyMessagesConfiguredGroup(t *testing.T) {
	f := newFakeLarkServer(t)
	s := testServerWithAuth(t)
	withLocations(t, s, f.ts.URL+"/open-apis")
	withSFGroup(t, s, "group")
	h := s.Handler()
	rec := adminPost(t, h, s, "/admin/locations/test-group", url.Values{"id": {"loc-sf"}, "chat_id": {"oc_attacker"}, "lark_chat_id": {"oc_attacker"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "group=") {
		t.Fatalf("want success, got %s", loc)
	}
	if got := f.got(); len(got) != 1 || got[0] != "chat_id:oc_sf_group" {
		t.Fatalf("test message went to %v, want only the SF group", got)
	}
	if body := adminGet(t, h, s, "/admin"); !strings.Contains(body, "Everyone in it will see it") {
		t.Error("Send test to group must warn that the group will see it")
	}
	rec = adminPost(t, h, s, "/admin/locations/test-group", url.Values{"id": {"loc-hq"}})
	if !strings.Contains(rec.Header().Get("Location"), "error") || len(f.got()) != 1 {
		t.Errorf("a location with no group must error and send nothing: %s, sent %v", rec.Header().Get("Location"), f.got())
	}
}
