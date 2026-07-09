package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// deviceAuth maps device_id -> expected token sha256 (hex), built once at startup.
type deviceAuth struct {
	want map[string][]byte // device_id -> 32-byte sha256
}

func newDeviceAuth(cfg *config.Config) *deviceAuth {
	a := &deviceAuth{want: make(map[string][]byte, len(cfg.Rooms))}
	for _, r := range cfg.Rooms {
		if b, err := hex.DecodeString(r.TokenSHA256); err == nil && len(b) == 32 {
			a.want[r.DeviceID] = b
		}
	}
	return a
}

// verify checks the bearer token against the stored hash in constant time. It returns true
// only if the token belongs to exactly this device_id (prevents a leaked token from one room
// being replayed against another).
func (a *deviceAuth) verify(deviceID string, r *http.Request) bool {
	want, ok := a.want[deviceID]
	if !ok {
		return false
	}
	auth := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(auth, p) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(auth, p)))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}
