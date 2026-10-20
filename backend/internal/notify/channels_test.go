package notify

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeBrowser is the user-agent side of a push subscription: it holds the private key, so it can
// decrypt what the broker sends — exactly what a real browser does.
type fakeBrowser struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return &fakeBrowser{priv: priv, auth: auth}
}

func (b *fakeBrowser) sub(endpoint string) PushSubscription {
	return PushSubscription{Endpoint: endpoint,
		P256DH: base64.RawURLEncoding.EncodeToString(b.priv.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(b.auth)}
}

// decrypt parses an aes128gcm body (RFC 8188 header) and runs RFC 8291 from the receiving side.
func (b *fakeBrowser) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	if rs := binary.BigEndian.Uint32(body[16:20]); rs != 4096 {
		t.Fatalf("record size %d", rs)
	}
	asPub := body[21 : 21+idlen]
	peer, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	cek, nonce, err := pushKeys(b.priv, peer, b.auth, salt, asPub, b.priv.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if pt[len(pt)-1] != 0x02 {
		t.Fatalf("missing last-record delimiter")
	}
	return pt[:len(pt)-1]
}

type memSubs struct {
	mu      sync.Mutex
	subs    []PushSubscription
	removed []string
}

func (m *memSubs) PushSubscriptions(string) []PushSubscription {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]PushSubscription(nil), m.subs...)
}

func (m *memSubs) RemovePushSubscription(endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, endpoint)
}

func testVAPID(t *testing.T) *VAPIDKeys {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	keys, err := ParseVAPIDKeys(base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(k.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestWebPushDeliversDecryptablePayloadWithVAPID(t *testing.T) {
	browser := newFakeBrowser(t)
	var got []byte
	var hdr http.Header
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	keys := testVAPID(t)
	wp := NewWebPush(keys, "mailto:ops@example.com", &memSubs{subs: []PushSubscription{browser.sub(ts.URL + "/push/abc")}})
	wp.Client = ts.Client()
	if err := wp.Send(context.Background(), Message{Title: "Low battery: Bias", Text: "40%", DeviceID: "rt-bias"}); err != nil {
		t.Fatal(err)
	}

	var p pushPayload
	if err := json.Unmarshal(browser.decrypt(t, got), &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "Low battery: Bias" || p.Body != "40%" || p.Tag != "rt-bias" {
		t.Errorf("payload = %+v", p)
	}
	if hdr.Get("Content-Encoding") != "aes128gcm" {
		t.Errorf("Content-Encoding = %q", hdr.Get("Content-Encoding"))
	}
	auth := hdr.Get("Authorization")
	if !strings.HasPrefix(auth, "vapid t=") || !strings.Contains(auth, ", k="+keys.PublicKey()) {
		t.Errorf("Authorization header not VAPID: %q", auth)
	}
}

// 404/410 from the push service = subscription gone: remove it, don't report an error. Other
// failures are errors and the subscription is kept.
func TestWebPushRemovesGoneSubscriptionsOnly(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(http.StatusGone)
		case strings.HasSuffix(r.URL.Path, "/missing"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()
	b := newFakeBrowser(t)
	subs := &memSubs{subs: []PushSubscription{b.sub(ts.URL + "/gone"), b.sub(ts.URL + "/missing"), b.sub(ts.URL + "/broken")}}
	wp := NewWebPush(testVAPID(t), "mailto:ops@example.com", subs)
	wp.Client = ts.Client()

	err := wp.Send(context.Background(), Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("want the 500 reported, got %v", err)
	}
	if len(subs.removed) != 2 || !strings.HasSuffix(subs.removed[0], "/gone") || !strings.HasSuffix(subs.removed[1], "/missing") {
		t.Errorf("removed = %v, want exactly the 410 and 404 endpoints", subs.removed)
	}
}

func TestParseVAPIDKeysRejectsMismatchedPair(t *testing.T) {
	a, _ := ecdh.P256().GenerateKey(rand.Reader)
	b, _ := ecdh.P256().GenerateKey(rand.Reader)
	_, err := ParseVAPIDKeys(base64.RawURLEncoding.EncodeToString(a.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(b.Bytes()))
	if err == nil {
		t.Fatal("a private key from a different pair must be rejected at startup")
	}
}

type countingNotifier struct {
	mu   sync.Mutex
	msgs []Message
}

func (c *countingNotifier) Name() string { return "counting" }
func (c *countingNotifier) Send(_ context.Context, m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

// One "offline" per transition, a repeat only after renotify, one "recovered" — never one per check.
func TestEvaluateStaleHysteresis(t *testing.T) {
	m := NewManagerWith(&countingNotifier{}, 45, 55, 24*time.Hour, quietLog())
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	steps := []struct {
		stale bool
		at    time.Duration
		want  StaleEvent
	}{
		{false, 0, StaleNone},
		{true, time.Minute, StaleOffline},
		{true, 2 * time.Minute, StaleNone}, // still offline: no repeat
		{true, 23 * time.Hour, StaleNone},
		{true, 25 * time.Hour, StaleOffline}, // renotify window elapsed
		{false, 26 * time.Hour, StaleRecovered},
		{false, 27 * time.Hour, StaleNone}, // still fine: no repeat
		{true, 28 * time.Hour, StaleOffline},
	}
	for i, s := range steps {
		if got := m.EvaluateStale("rt-a", "A", s.stale, time.Hour, t0.Add(s.at)); got != s.want {
			t.Errorf("step %d (stale=%v at +%s): got %v, want %v", i, s.stale, s.at, got, s.want)
		}
	}
}

// Every selected channel gets the message even if an earlier one fails.
func TestMultiAttemptsEveryChannel(t *testing.T) {
	a, b := &countingNotifier{}, &countingNotifier{}
	failing := NewLarkApp("cli_x", "secret", "http://127.0.0.1:1", nil) // refused
	err := Multi{failing, a, b}.Send(context.Background(), Message{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "lark") {
		t.Errorf("want the lark failure attributed, got %v", err)
	}
	if len(a.msgs) != 1 || len(b.msgs) != 1 {
		t.Errorf("channels after a failing one must still send: %d, %d", len(a.msgs), len(b.msgs))
	}
}

// RFC 8291 Appendix A: fixed keys and salt must reproduce the RFC's exact encrypted body. This
// pins the key schedule independently of the round-trip test above, which shares pushKeys with
// the sender and so couldn't catch a symmetric mistake (e.g. key_info fields swapped).
func TestEncryptMatchesRFC8291AppendixA(t *testing.T) {
	d := func(s string) []byte { b, _ := base64.RawURLEncoding.DecodeString(s); return b }
	asPriv, err := ecdh.P256().NewPrivateKey(d("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPub := d("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	ua, _ := ecdh.P256().NewPublicKey(uaPub)
	salt, authSecret := d("DGv6ra1nlYgDCS1FRnbzlw"), d("BTBZMqHH6r4Tts7J_aSIgg")
	asPub := asPriv.PublicKey().Bytes()

	cek, nonce, err := pushKeys(asPriv, ua, authSecret, salt, asPub, uaPub)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	ct := gcm.Seal(nil, nonce, append([]byte("When I grow up, I want to be a watermelon"), 0x02), nil)
	body := append(append(append(append([]byte{}, salt...), 0, 0, 0x10, 0, byte(len(asPub))), asPub...), ct...)

	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if got := base64.RawURLEncoding.EncodeToString(body); got != want {
		t.Errorf("RFC 8291 A body mismatch:\n got %s\nwant %s", got, want)
	}
}

// Every failure carries the push service's status and a redacted reason; nothing delivered is
// an error (the old code returned nil when every subscription 404/410'd, so callers said "sent").
func TestWebPushReportsStatusAndFailsWhenNothingDelivered(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"reason":"the key in the authorization header does not correspond to the sender ID used to subscribe ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop0123456789"}`))
	}))
	defer ts.Close()
	b := newFakeBrowser(t)
	wp := NewWebPush(testVAPID(t), "mailto:ops@example.com", &memSubs{subs: []PushSubscription{b.sub(ts.URL + "/secret-endpoint-path")}})
	wp.Client = ts.Client()
	err := wp.Send(context.Background(), Message{Title: "x"})
	if err == nil {
		t.Fatal("nothing was delivered; Send must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTP 403") || !strings.Contains(msg, "does not correspond") {
		t.Errorf("status/reason missing: %s", msg)
	}
	for _, leak := range []string{"secret-endpoint-path", "ABCDEFGHIJKLMNOPQRSTUVWXYZ"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error leaks %q: %s", leak, msg)
		}
	}
	if (&memSubs{}).PushSubscriptions("") == nil && wp.SendReport(context.Background(), Message{}).Total != 1 {
		t.Error("report total wrong")
	}
	empty := NewWebPush(testVAPID(t), "mailto:ops@example.com", &memSubs{})
	if r := empty.SendReport(context.Background(), Message{}); r.Total != 0 || empty.Send(context.Background(), Message{}) != nil {
		t.Error("no subscriptions is not a failure for alerts")
	}
}

// A 404/410 on a subscription created moments ago is a sender-side smell: keep it and say so.
// Older ones are deleted as before.
func TestWebPushKeepsJustCreatedSubscriptionOnGone(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte("push subscription has unsubscribed or expired."))
	}))
	defer ts.Close()
	now := time.Date(2026, 10, 20, 2, 9, 44, 0, time.UTC)
	b := newFakeBrowser(t)
	fresh, old := b.sub(ts.URL+"/fresh"), b.sub(ts.URL+"/old")
	fresh.Created, old.Created = now.Add(-3*time.Second), now.Add(-48*time.Hour)
	subs := &memSubs{subs: []PushSubscription{fresh, old}}
	var logBuf strings.Builder
	wp := NewWebPush(testVAPID(t), "mailto:ops@example.com", subs)
	wp.Client, wp.now, wp.Log = ts.Client(), func() time.Time { return now }, slog.New(slog.NewTextHandler(&logBuf, nil))
	r := wp.SendReport(context.Background(), Message{Title: "x"})
	if r.Kept != 1 || r.Removed != 1 || len(subs.removed) != 1 || !strings.HasSuffix(subs.removed[0], "/old") {
		t.Fatalf("report=%+v removed=%v: want the fresh one kept and only the old one deleted", r, subs.removed)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "status=410") {
		t.Errorf("keeping a just-created subscription must be logged loudly with the status:\n%s", logBuf.String())
	}
}
