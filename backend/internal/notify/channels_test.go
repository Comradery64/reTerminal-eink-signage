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

func (m *memSubs) PushSubscriptions() []PushSubscription {
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

func TestLarkWebhookChecksBodyVerdict(t *testing.T) {
	var gotText string
	reply := `{"code":0,"msg":"success"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MsgType string `json:"msg_type"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotText = body.MsgType + ":" + body.Content.Text
		_, _ = w.Write([]byte(reply))
	}))
	defer ts.Close()
	l := NewLarkWebhook(ts.URL)
	if err := l.Send(context.Background(), Message{Title: "T", Text: "body"}); err != nil {
		t.Fatal(err)
	}
	if gotText != "text:T\nbody" {
		t.Errorf("lark payload = %q", gotText)
	}
	reply = `{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}`
	if err := l.Send(context.Background(), Message{Title: "T"}); err == nil {
		t.Error("HTTP 200 with a non-zero code is a rejection and must be an error")
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
	failing := NewLarkWebhook("http://127.0.0.1:1") // refused
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
