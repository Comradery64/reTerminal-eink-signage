package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/nvs"
)

// nvsPartitionSize must match the `nvs` partition in firmware/partitions.csv (0x6000). Changing
// one without the other produces an image the device either can't fit or can't fully read.
const nvsPartitionSize = 0x6000

// provisionNVSResponse is what the "Add a device" wizard needs to finish flashing a blank unit:
// the partition image to write at offset 0x9000, and the hash to store against the room.
//
// NVSBase64 contains the device's plaintext bearer token *and* the fleet Wi-Fi PSK, because that
// is exactly what has to end up in the unit's flash. It is therefore write-only from the
// browser's point of view: the wizard pipes it straight to WebSerial and must never render,
// store, or re-POST it. Everything the wizard sends back to the broker is TokenSHA256.
type provisionNVSResponse struct {
	NVSBase64   string `json:"nvs_b64"`
	NVSOffset   int    `json:"nvs_offset"` // 0x9000, so the client doesn't hardcode the partition map
	TokenSHA256 string `json:"token_sha256"`
}

// handleProvisionNVS builds a fresh NVS provisioning image for one device.
//
// The fleet Wi-Fi PSK never leaves the broker in any other form. An earlier sketch of this
// wizard had the browser fetch the PSK and assemble the image client-side; that would have made
// a shared, fleet-wide secret readable by anything running in an admin's tab, which is a far
// worse trade than doing the binary-format work in Go (see internal/nvs).
//
// Each call mints a new token. There is deliberately no way to retrieve a device's existing
// token: the broker only ever stores the SHA-256, so reprovisioning a unit means reflashing it.
func (s *Server) handleProvisionNVS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad JSON body", http.StatusBadRequest)
		return
	}
	if req.DeviceID == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}

	cfg := s.cfg.Load()
	if cfg.Fleet.WifiSSID == "" || cfg.Fleet.WifiPSK == "" {
		// A clear precondition failure, because the alternative is flashing a unit that silently
		// never joins the network. Don't say which of the two is missing beyond this.
		s.log.Error("provision-nvs called with no fleet Wi-Fi configured", "device", req.DeviceID)
		http.Error(w, "fleet Wi-Fi credentials are not configured; set fleet.wifi_ssid and fleet.wifi_psk", http.StatusPreconditionFailed)
		return
	}

	// 32 bytes of entropy, base64url without padding — full 256 bits, unlike
	// tools/provision_device.sh's strip-and-truncate scheme. The token is opaque to the firmware,
	// so the two schemes coexist fine across a mixed fleet.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		s.log.Error("provision-nvs token generation failed", "device", req.DeviceID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	img, err := nvs.Build(nvsPartitionSize, "prov", []nvs.KV{
		{Key: "device_id", Value: req.DeviceID},
		{Key: "token", Value: token},
		{Key: "wifi_ssid", Value: cfg.Fleet.WifiSSID},
		{Key: "wifi_psk", Value: cfg.Fleet.WifiPSK},
	})
	if err != nil {
		// Logged without the error's operands: nvs.Build's messages quote the offending key, never
		// a value, but keep the habit anyway on a path that handles two secrets.
		s.log.Error("provision-nvs image build failed", "device", req.DeviceID, "err", err)
		http.Error(w, "could not build the provisioning image: "+err.Error(), http.StatusBadRequest)
		return
	}

	// no-store, not just no-cache: this body holds two secrets and must not land in a disk cache
	// or a proxy on the way back to the browser.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(provisionNVSResponse{
		NVSBase64:   base64.StdEncoding.EncodeToString(img),
		NVSOffset:   0x9000,
		TokenSHA256: hex.EncodeToString(sum[:]),
	}); err != nil {
		s.log.Error("provision-nvs response write failed", "device", req.DeviceID, "err", err)
	}
}
