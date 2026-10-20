package notify

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Web Push (RFC 8030) with message encryption (RFC 8291, aes128gcm) and VAPID (RFC 8292),
// implemented on the standard library only: crypto/ecdh for the per-message key agreement,
// crypto/hkdf for the key schedule, AES-128-GCM for the record, ES256 for the VAPID JWT.

// PushSubscription is what a browser's PushManager.subscribe() returns: the push-service
// endpoint plus the user agent's P-256 public key and auth secret (both base64url).
type PushSubscription struct {
	Endpoint string
	P256DH   string
	Auth     string
	Created  time.Time // zero if unknown; used to refuse deleting a just-created subscription
}

// PushSubscriptions supplies the subscriptions that should receive an alert for a location ("" =
// no location routing) and is told when the push service reports one gone (404/410), so it can
// be deleted instead of retried forever.
type PushSubscriptions interface {
	PushSubscriptions(location string) []PushSubscription
	RemovePushSubscription(endpoint string)
}

// PushError is one push service's refusal: its HTTP status and its reason text (redacted of
// anything token-shaped). The endpoint path, keys and payload are never included.
type PushError struct {
	Origin string // scheme://host of the push service, e.g. https://fcm.googleapis.com
	Status int
	Reason string
}

func (e *PushError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("%s answered HTTP %d", e.Origin, e.Status)
	}
	return fmt.Sprintf("%s answered HTTP %d: %s", e.Origin, e.Status, e.Reason)
}

// PushReport is what one Send actually achieved, so callers (the /admin test button) can say
// "sent", "failed", or "no subscriptions" truthfully instead of assuming success.
type PushReport struct {
	Total     int
	Delivered int
	Removed   int          // gone (404/410) and deleted
	Kept      int          // gone but NOT deleted because the subscription was just created
	Failures  []*PushError // every non-2xx, including the 404/410s above
}

// freshSubscriptionGrace: a push service rejecting a subscription this young is almost never
// "the user unsubscribed" — it points at a sender-side problem (keys, JWT, encoding) — so it's
// logged loudly and kept rather than silently deleted.
const freshSubscriptionGrace = 10 * time.Minute

// VAPIDKeys is a P-256 application-server key pair in the standard web-push encoding:
// public = base64url of the 65-byte uncompressed point, private = base64url of the 32-byte scalar.
type VAPIDKeys struct {
	public  []byte
	private *ecdsa.PrivateKey
}

// ParseVAPIDKeys decodes and cross-checks a key pair: the private key must derive the given
// public key, so a mismatched pair fails at startup rather than at the first (rejected) push.
func ParseVAPIDKeys(publicB64, privateB64 string) (*VAPIDKeys, error) {
	pub, err := b64url(publicB64)
	if err != nil || len(pub) != 65 || pub[0] != 4 {
		return nil, errors.New("vapid public key must be base64url of a 65-byte uncompressed P-256 point")
	}
	d, err := b64url(privateB64)
	if err != nil || len(d) != 32 {
		return nil, errors.New("vapid private key must be base64url of a 32-byte P-256 scalar")
	}
	ek, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return nil, fmt.Errorf("vapid private key: %w", err)
	}
	if !bytes.Equal(ek.PublicKey().Bytes(), pub) {
		return nil, errors.New("vapid private key does not match the public key")
	}
	priv := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(pub[1:33]), Y: new(big.Int).SetBytes(pub[33:])},
		D:         new(big.Int).SetBytes(d),
	}
	return &VAPIDKeys{public: pub, private: priv}, nil
}

// PublicKey returns the base64url public key a browser passes to subscribe() as
// applicationServerKey.
func (k *VAPIDKeys) PublicKey() string { return base64.RawURLEncoding.EncodeToString(k.public) }

// WebPush sends every Message to every current subscription.
type WebPush struct {
	Log     *slog.Logger // optional; reports subscriptions kept despite a 404/410
	Keys    *VAPIDKeys
	Subject string // VAPID "sub": mailto: or https: contact for the push service operator
	Subs    PushSubscriptions
	Client  *http.Client
	TTL     time.Duration
	now     func() time.Time
}

func NewWebPush(keys *VAPIDKeys, subject string, subs PushSubscriptions) *WebPush {
	return &WebPush{Keys: keys, Subject: subject, Subs: subs,
		Client: &http.Client{Timeout: 8 * time.Second}, TTL: 24 * time.Hour, now: time.Now}
}

func (w *WebPush) Name() string { return "webpush" }

// pushPayload is what the service worker receives (see server/push_sw.js).
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag,omitempty"` // same tag replaces an earlier notification for that room
}

// Send delivers m to every subscription for its location and errors unless it reached at least
// one browser (or there were none to reach). A definitive 404/410 deletes the subscription —
// unless it was created moments ago, which is logged loudly and kept (see freshSubscriptionGrace).
func (w *WebPush) Send(ctx context.Context, m Message) error {
	r := w.SendReport(ctx, m)
	if r.Total == 0 || r.Delivered > 0 && len(r.Failures) == 0 {
		return nil
	}
	errs := make([]error, 0, len(r.Failures))
	for _, f := range r.Failures {
		errs = append(errs, f)
	}
	if r.Delivered > 0 {
		return fmt.Errorf("delivered to %d of %d browsers: %w", r.Delivered, r.Total, errors.Join(errs...))
	}
	return fmt.Errorf("not delivered to any of %d browsers: %w", r.Total, errors.Join(errs...))
}

// SendReport is Send with the full outcome.
func (w *WebPush) SendReport(ctx context.Context, m Message) PushReport {
	payload, _ := json.Marshal(pushPayload{Title: m.Title, Body: m.Text, Tag: m.DeviceID})
	subs := w.Subs.PushSubscriptions(m.Location)
	r := PushReport{Total: len(subs)}
	for _, sub := range subs {
		status, reason, err := w.sendOne(ctx, sub, payload)
		switch {
		case err != nil:
			r.Failures = append(r.Failures, &PushError{Origin: endpointOrigin(sub.Endpoint), Reason: err.Error()})
		case status >= 200 && status < 300:
			r.Delivered++
		default:
			pe := &PushError{Origin: endpointOrigin(sub.Endpoint), Status: status, Reason: reason}
			r.Failures = append(r.Failures, pe)
			if status != http.StatusNotFound && status != http.StatusGone {
				continue
			}
			if !sub.Created.IsZero() && w.now().Sub(sub.Created) < freshSubscriptionGrace {
				r.Kept++
				if w.Log != nil {
					w.Log.Error("push service rejected a subscription created moments ago; keeping it — this points at a sender-side problem (keys, JWT, encoding), not an expired subscription",
						"push_service", pe.Origin, "status", status, "reason", reason, "age", w.now().Sub(sub.Created).Round(time.Second))
				}
				continue
			}
			w.Subs.RemovePushSubscription(sub.Endpoint)
			r.Removed++
		}
	}
	return r
}

func (w *WebPush) sendOne(ctx context.Context, sub PushSubscription, payload []byte) (status int, reason string, err error) {
	body, err := encryptPush(sub, payload)
	if err != nil {
		return 0, "", err
	}
	jwt, err := w.vapidJWT(sub.Endpoint)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", redactURL(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", fmt.Sprint(int(w.TTL.Seconds())))
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+w.Keys.PublicKey())
	resp, err := w.Client.Do(req)
	if err != nil {
		return 0, "", redactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return resp.StatusCode, pushReason(raw), nil
	}
	return resp.StatusCode, "", nil
}

var tokenShaped = regexp.MustCompile(`[A-Za-z0-9_\-:.]{40,}`)

// pushReason condenses a push service error body to one safe line: FCM/Mozilla/Apple put the
// human reason in JSON or plain text; anything long and token-like (it can echo the
// registration ID) is redacted, and the result is capped.
func pushReason(raw []byte) string {
	var j struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	s := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &j) == nil {
		switch {
		case j.Reason != "":
			s = j.Reason
		case j.Message != "":
			s = j.Message
		case j.Error != nil:
			b, _ := json.Marshal(j.Error)
			s = string(b)
		}
	}
	s = tokenShaped.ReplaceAllString(strings.Join(strings.Fields(s), " "), "<redacted>")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// vapidJWT signs the RFC 8292 claims: aud = the push service origin, exp <= 24h, sub = contact.
func (w *WebPush) vapidJWT(endpoint string) (string, error) {
	aud := endpointOrigin(endpoint)
	if aud == "" {
		return "", fmt.Errorf("bad push endpoint")
	}
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": w.now().Add(12 * time.Hour).Unix(), "sub": w.Subject})
	signing := hdr + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, w.Keys.private, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS ES256 = fixed-width r||s, not ASN.1
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// encryptPush builds one aes128gcm record per RFC 8291 §3–4 / RFC 8188 §2.
func encryptPush(sub PushSubscription, plaintext []byte) ([]byte, error) {
	uaPub, err := b64url(sub.P256DH)
	if err != nil {
		return nil, fmt.Errorf("subscription p256dh: %w", err)
	}
	authSecret, err := b64url(sub.Auth)
	if err != nil || len(authSecret) != 16 {
		return nil, errors.New("subscription auth secret must be 16 bytes")
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("subscription p256dh: %w", err)
	}
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	cek, nonce, err := pushKeys(asKey, uaKey, authSecret, salt, asKey.PublicKey().Bytes(), uaPub)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	record := append(append([]byte{}, plaintext...), 0x02) // 0x02 = last-record delimiter, no padding
	ct := gcm.Seal(nil, nonce, record, nil)

	asPub := asKey.PublicKey().Bytes()
	hdr := make([]byte, 0, 16+4+1+len(asPub))
	hdr = append(hdr, salt...)
	hdr = binary.BigEndian.AppendUint32(hdr, 4096) // record size; one record always fits
	hdr = append(hdr, byte(len(asPub)))
	hdr = append(hdr, asPub...)
	return append(hdr, ct...), nil
}

// pushKeys runs the RFC 8291 §3.4 key schedule. Shared by encryptPush and the tests' decryptor,
// which drives it from the user-agent side (its private key, the sender's public key) — ECDH is
// symmetric, so both sides must derive the same CEK and nonce.
func pushKeys(priv *ecdh.PrivateKey, peer *ecdh.PublicKey, authSecret, salt, asPub, uaPub []byte) (cek, nonce []byte, err error) {
	shared, err := priv.ECDH(peer)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

func b64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// endpointOrigin returns scheme://host of a push endpoint — the VAPID audience, and the only
// part of an endpoint safe to log (the path is a per-browser capability).
func endpointOrigin(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
