package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const totpStep = 30 * time.Second

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh random base32 secret (160 bits, the size every authenticator app
// expects) suitable for enrolling one account in TOTP.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return base32NoPad.EncodeToString(b)
}

// TOTPCode computes the RFC 6238 code for secret at t — 6 digits, 30-second step, HMAC-SHA1,
// the parameters every authenticator app (Google Authenticator, Authy, 1Password, etc.) assumes
// when an otpauth:// URI doesn't say otherwise.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := base32NoPad.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(t.Unix())/uint64(totpStep.Seconds()))

	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code), nil
}

// VerifyTOTP checks code against secret at now, tolerating one 30-second step of clock drift in
// either direction — real devices and phones are rarely perfectly in sync.
func VerifyTOTP(secret, code string, now time.Time) bool {
	for _, skew := range [3]int{0, -1, 1} {
		want, err := TOTPCode(secret, now.Add(time.Duration(skew)*totpStep))
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// TOTPURI builds the otpauth:// URI an authenticator app can import (typed in manually, or via
// whatever "enter setup key" / paste-URI option the app offers — no QR code image needed).
func TOTPURI(issuer, account, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&digits=6&period=30",
		issuer, account, secret, issuer)
}
