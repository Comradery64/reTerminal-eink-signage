package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeAppSecret = "app-secret-DO-NOT-LEAK"
	fakeToken     = "t-tenant-token-DO-NOT-LEAK"
)

// fakeLark is a local stand-in for the Lark Open API: it issues tenant tokens, records messages,
// and can be told to reject DMs or tokens with the HTTP-200-plus-nonzero-code Lark uses.
type fakeLark struct {
	mu          sync.Mutex
	tokenCalls  int
	sent        []sentMsg
	rejectEmail bool            // DMs fail with a nonzero code
	rejectChats map[string]bool // chat IDs whose messages fail with a nonzero code
	tokenExpire int
	badTokenNow bool // next message gets "token invalid"
}

type sentMsg struct{ IDType, ID, Text, Auth string }

func (f *fakeLark) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			var body struct{ AppID, AppSecret string }
			var raw map[string]string
			_ = json.NewDecoder(r.Body).Decode(&raw)
			body.AppID, body.AppSecret = raw["app_id"], raw["app_secret"]
			f.tokenCalls++
			if body.AppSecret != fakeAppSecret {
				_, _ = w.Write([]byte(`{"code":10014,"msg":"app secret invalid"}`))
				return
			}
			exp := f.tokenExpire
			if exp == 0 {
				exp = 7200
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": fakeToken, "expire": exp})
		case r.URL.Path == "/open-apis/im/v1/messages":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			var content map[string]string
			_ = json.Unmarshal([]byte(body["content"]), &content)
			idType := r.URL.Query().Get("receive_id_type")
			if f.badTokenNow {
				f.badTokenNow = false
				_, _ = w.Write([]byte(`{"code":99991663,"msg":"tenant access token invalid"}`))
				return
			}
			if idType == "email" && f.rejectEmail {
				_, _ = w.Write([]byte(`{"code":230013,"msg":"bot has no availability to this user"}`))
				return
			}
			if idType == "chat_id" && f.rejectChats[body["receive_id"]] {
				_, _ = w.Write([]byte(`{"code":230002,"msg":"bot not in chat"}`))
				return
			}
			f.sent = append(f.sent, sentMsg{idType, body["receive_id"], content["text"], r.Header.Get("Authorization")})
			_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

type routes struct {
	m        map[string][2]string // location -> {email, chat}
	mode     map[string]string    // location -> delivery mode ("" = original behaviour)
	fallback string
}

func (r routes) LarkRoute(loc string) (string, string) { v := r.m[loc]; return v[0], v[1] }
func (r routes) LarkDelivery(loc string) string        { return r.mode[loc] }
func (r routes) LarkFallbackChat() string              { return r.fallback }

func newTestApp(t *testing.T, f *fakeLark, rt LarkRoutes) (*LarkApp, func()) {
	ts := httptest.NewServer(f.handler(t))
	return NewLarkApp("cli_test", fakeAppSecret, ts.URL+"/open-apis", rt), ts.Close
}

// The location's person gets a DM; the fallback chat is NOT also messaged.
func TestLarkAppDMsLocationPersonOnly(t *testing.T) {
	f := &fakeLark{}
	app, done := newTestApp(t, f, routes{m: map[string][2]string{"loc-a": {"a@example.com", ""}}, fallback: "oc_fallback"})
	defer done()
	if err := app.Send(context.Background(), Message{Title: "Low battery", Text: "Room 1 at 40%", Location: "loc-a"}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0].IDType != "email" || f.sent[0].ID != "a@example.com" || f.sent[0].Text != "Low battery\nRoom 1 at 40%" {
		t.Fatalf("sent = %+v, want exactly one DM to the location's person", f.sent)
	}
	if f.sent[0].Auth != "Bearer "+fakeToken {
		t.Error("message not authorized with the tenant token")
	}
}

// DM rejected (HTTP 200 + nonzero code) → falls back to the chat, once.
func TestLarkAppFallsBackOnlyWhenDMFails(t *testing.T) {
	f := &fakeLark{rejectEmail: true}
	app, done := newTestApp(t, f, routes{m: map[string][2]string{"loc-a": {"a@example.com", ""}}, fallback: "oc_fallback"})
	defer done()
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc-a"}); err != nil {
		t.Fatalf("fallback delivered, so Send should succeed: %v", err)
	}
	if len(f.sent) != 1 || f.sent[0].IDType != "chat_id" || f.sent[0].ID != "oc_fallback" {
		t.Fatalf("sent = %+v, want one message to the fallback chat", f.sent)
	}
}

// No person: the location's own chat wins over the global fallback.
func TestLarkAppNoPersonUsesLocationChatThenGlobal(t *testing.T) {
	f := &fakeLark{}
	app, done := newTestApp(t, f, routes{m: map[string][2]string{"loc-b": {"", "oc_locb"}}, fallback: "oc_fallback"})
	defer done()
	_ = app.Send(context.Background(), Message{Title: "x", Location: "loc-b"})
	_ = app.Send(context.Background(), Message{Title: "y", Location: "unknown"})
	if len(f.sent) != 2 || f.sent[0].ID != "oc_locb" || f.sent[1].ID != "oc_fallback" {
		t.Fatalf("sent = %+v", f.sent)
	}
}

// One tenant token is reused until near expiry, refreshed early, and re-fetched once if Lark
// says it's invalid.
func TestLarkAppTokenCacheAndRefresh(t *testing.T) {
	f := &fakeLark{}
	app, done := newTestApp(t, f, routes{m: map[string][2]string{"l": {"a@example.com", ""}}})
	defer done()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	send := func() { _ = app.Send(context.Background(), Message{Title: "x", Location: "l"}) }

	send()
	send()
	if f.tokenCalls != 1 {
		t.Fatalf("token fetched %d times for 2 sends, want 1 (cached)", f.tokenCalls)
	}
	now = now.Add(2*time.Hour - 4*time.Minute) // inside the 5-min refresh margin
	send()
	if f.tokenCalls != 2 {
		t.Fatalf("token not refreshed early: %d calls", f.tokenCalls)
	}
	f.badTokenNow = true
	send()
	if f.tokenCalls != 3 || len(f.sent) != 4 {
		t.Fatalf("invalid token should trigger one refetch+retry: calls=%d sent=%d", f.tokenCalls, len(f.sent))
	}
}

// Errors never carry the app secret, the tenant token, or the request URL.
func TestLarkAppErrorsLeakNothing(t *testing.T) {
	f := &fakeLark{rejectEmail: true}
	app, done := newTestApp(t, f, routes{m: map[string][2]string{"l": {"a@example.com", ""}}})
	defer done()
	err := app.Send(context.Background(), Message{Title: "x", Location: "l"})
	if err == nil || !strings.Contains(err.Error(), "230013") {
		t.Fatalf("want the Lark code reported, got %v", err)
	}
	bad := NewLarkApp("cli_test", "wrong-secret", app.BaseURL, nil)
	err2 := bad.SendDM(context.Background(), "a@example.com", Message{Title: "x"})
	refused := NewLarkApp("cli_test", fakeAppSecret, "http://127.0.0.1:1/open-apis/secret-path", nil)
	err3 := refused.SendDM(context.Background(), "a@example.com", Message{Title: "x"})
	for _, e := range []error{err, err2, err3} {
		if e == nil {
			t.Fatal("expected an error")
		}
		for _, s := range []string{fakeAppSecret, fakeToken, "wrong-secret", "secret-path", "127.0.0.1:1/open-apis"} {
			if strings.Contains(e.Error(), s) {
				t.Errorf("error %q leaks %q", e, s)
			}
		}
	}
}

func sentIDs(f *fakeLark) []string {
	var ids []string
	for _, m := range f.sent {
		ids = append(ids, m.ID)
	}
	return ids
}

func deliveryApp(t *testing.T, f *fakeLark, mode string) (*LarkApp, func()) {
	return newTestApp(t, f, routes{
		m:        map[string][2]string{"loc": {"a@example.com", "oc_loc"}},
		mode:     map[string]string{"loc": mode},
		fallback: "oc_fallback",
	})
}

// delivery=group: the group only. The person is not messaged even though one is configured.
func TestLarkAppGroupModeSkipsTheDM(t *testing.T) {
	f := &fakeLark{}
	app, done := deliveryApp(t, f, "group")
	defer done()
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc"}); err != nil {
		t.Fatal(err)
	}
	if got := sentIDs(f); len(got) != 1 || got[0] != "oc_loc" {
		t.Fatalf("sent to %v, want only the location's group", got)
	}
}

// delivery=group: the global fallback is used only if the location's group fails.
func TestLarkAppGroupModeFallsBackOnlyOnFailure(t *testing.T) {
	f := &fakeLark{rejectChats: map[string]bool{"oc_loc": true}}
	app, done := deliveryApp(t, f, "group")
	defer done()
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc"}); err != nil {
		t.Fatalf("fallback delivered, so Send should succeed: %v", err)
	}
	if got := sentIDs(f); len(got) != 1 || got[0] != "oc_fallback" {
		t.Fatalf("sent to %v, want only the fallback (never a DM)", got)
	}
}

// delivery=both: person and group each get one message and the fallback stays silent.
func TestLarkAppBothModeMessagesBoth(t *testing.T) {
	f := &fakeLark{}
	app, done := deliveryApp(t, f, "both")
	defer done()
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc"}); err != nil {
		t.Fatal(err)
	}
	if got := sentIDs(f); len(got) != 2 || got[0] != "a@example.com" || got[1] != "oc_loc" {
		t.Fatalf("sent to %v, want the person then the group", got)
	}
}

// delivery=both: one side failing is not a failure and does not trigger the fallback; only both
// failing does.
func TestLarkAppBothModeFallbackNeedsBothToFail(t *testing.T) {
	f := &fakeLark{rejectEmail: true}
	app, done := deliveryApp(t, f, "both")
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc"}); err != nil {
		t.Fatal(err)
	}
	if got := sentIDs(f); len(got) != 1 || got[0] != "oc_loc" {
		t.Fatalf("DM failed but the group worked: sent to %v, want only the group", got)
	}
	done()

	f = &fakeLark{rejectEmail: true, rejectChats: map[string]bool{"oc_loc": true}}
	app, done = deliveryApp(t, f, "both")
	defer done()
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc"}); err != nil {
		t.Fatal(err)
	}
	if got := sentIDs(f); len(got) != 1 || got[0] != "oc_fallback" {
		t.Fatalf("both failed: sent to %v, want only the fallback", got)
	}
}

// delivery=dm: the person only; if the DM fails the location's group is tried before the global one.
func TestLarkAppDMModeFailsOverToLocationGroup(t *testing.T) {
	f := &fakeLark{}
	app, done := deliveryApp(t, f, "dm")
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc"}); err != nil {
		t.Fatal(err)
	}
	if got := sentIDs(f); len(got) != 1 || got[0] != "a@example.com" {
		t.Fatalf("sent to %v, want only the DM", got)
	}
	done()

	f = &fakeLark{rejectEmail: true}
	app, done = deliveryApp(t, f, "dm")
	defer done()
	if err := app.Send(context.Background(), Message{Title: "x", Location: "loc"}); err != nil {
		t.Fatal(err)
	}
	if got := sentIDs(f); len(got) != 1 || got[0] != "oc_loc" {
		t.Fatalf("DM failed: sent to %v, want the location's group, not the global fallback", got)
	}
}

// Everything failing is reported, and the error names the location but no secret.
func TestLarkAppReportsWhenNothingDelivers(t *testing.T) {
	f := &fakeLark{rejectEmail: true, rejectChats: map[string]bool{"oc_loc": true, "oc_fallback": true}}
	app, done := deliveryApp(t, f, "both")
	defer done()
	err := app.Send(context.Background(), Message{Title: "x", Location: "loc"})
	if err == nil || !strings.Contains(err.Error(), "230002") || strings.Contains(err.Error(), fakeAppSecret) {
		t.Fatalf("want a delivery error carrying the Lark code, got %v", err)
	}
}

// SendChat posts to exactly the chat it is given.
func TestLarkAppSendChat(t *testing.T) {
	f := &fakeLark{}
	app, done := newTestApp(t, f, nil)
	defer done()
	if err := app.SendChat(context.Background(), "oc_x", Message{Title: "t", Text: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0].IDType != "chat_id" || f.sent[0].ID != "oc_x" {
		t.Fatalf("sent = %+v", f.sent)
	}
}

// A message with no body is sent as just its title, with no trailing blank line.
func TestLarkAppSendsTitleOnlyMessageWithoutTrailingNewline(t *testing.T) {
	f := &fakeLark{}
	app, done := newTestApp(t, f, routes{m: map[string][2]string{"loc": {"a@example.com", ""}}})
	defer done()
	if err := app.Send(context.Background(), Message{Title: "✅ Aspen: back online", Location: "loc"}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0].Text != "✅ Aspen: back online" {
		t.Fatalf("sent = %+v", f.sent)
	}
}
