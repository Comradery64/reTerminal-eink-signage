package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/nvs"
)

// Offsets and sizes below are fixed by firmware/partitions.csv and the ESP32-S3's boot ROM.
// Changing either without the other produces a unit that doesn't boot.
const (
	nvsPartitionSize = 0x6000

	offsetBootloader     = 0x0     // ESP32-S3 boots from 0; other targets use 0x1000
	offsetPartitionTable = 0x8000  // partition table
	offsetNVS            = 0x9000  // `nvs` partition — the provisioning secrets
	offsetApp            = 0x20000 // `ota_0`, the first app slot

	// provisionTTL bounds how long a minted image stays fetchable. Long enough for an operator to
	// pick a serial port and sit through a flash, short enough that an abandoned wizard tab
	// doesn't leave a credential-bearing URL live. Expired entries are also swept on each mint.
	provisionTTL = 15 * time.Minute
)

// firmwareImages are the three build artifacts the wizard flashes alongside the NVS image, in
// ascending flash offset. Names match what `idf.py build` produces for project(meeting_display).
var firmwareImages = []struct {
	File   string
	Offset int
}{
	{"bootloader.bin", offsetBootloader},
	{"partition-table.bin", offsetPartitionTable},
	{"meeting_display.bin", offsetApp},
}

// provisionArtifact is one minted, not-yet-flashed device image.
//
// It holds the device's plaintext bearer token and the fleet Wi-Fi PSK, so it lives in memory
// only, is reachable solely through an unguessable nonce behind the admin session, and expires.
// It is deliberately never written to disk and never logged.
type provisionArtifact struct {
	deviceID string
	nvs      []byte
	expires  time.Time
}

// provisionStore holds artifacts between the mint call and ESP Web Tools fetching them.
type provisionStore struct {
	mu sync.Mutex
	m  map[string]*provisionArtifact
}

func newProvisionStore() *provisionStore { return &provisionStore{m: map[string]*provisionArtifact{}} }

func (p *provisionStore) put(nonce string, a *provisionArtifact) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Sweep on write; there's no background goroutine to leak, and the map only ever holds as
	// many entries as there were mints in the last TTL window.
	now := time.Now()
	for k, v := range p.m {
		if now.After(v.expires) {
			delete(p.m, k)
		}
	}
	p.m[nonce] = a
}

func (p *provisionStore) get(nonce string) (*provisionArtifact, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.m[nonce]
	if !ok {
		return nil, false
	}
	if time.Now().After(a.expires) {
		delete(p.m, nonce)
		return nil, false
	}
	return a, true
}

// provisionMintResponse is what the wizard's JavaScript receives. Note what is absent: the
// plaintext token and the Wi-Fi PSK. The browser gets a manifest URL, hands it to ESP Web Tools,
// and the flasher fetches the image bytes same-origin under the admin's own session — so no
// credential is ever a JavaScript value, and none can be read out of the page.
type provisionMintResponse struct {
	ManifestURL string `json:"manifest_url"`
	TokenSHA256 string `json:"token_sha256"`
	ExpiresIn   int    `json:"expires_in_seconds"`
}

// espWebToolsManifest is the format ESP Web Tools' <esp-web-install-button> consumes.
// https://esphome.github.io/esp-web-tools/
type espWebToolsManifest struct {
	Name                  string             `json:"name"`
	Version               string             `json:"version"`
	NewInstallPromptErase bool               `json:"new_install_prompt_erase"`
	Builds                []espWebToolsBuild `json:"builds"`
}

type espWebToolsBuild struct {
	ChipFamily string            `json:"chipFamily"`
	Parts      []espWebToolsPart `json:"parts"`
	Improv     bool              `json:"improv"`
}

type espWebToolsPart struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
}

// firmwareDirOrError resolves the directory the three .bin build artifacts are served from,
// failing loudly when the deployment hasn't configured it — the wizard's most likely first-run
// stumble, and a 404 from the static handler would be a much worse way to discover it.
func (s *Server) firmwareDirOrError() (string, error) {
	dir := s.cfg.Load().Firmware.Dir
	if dir == "" {
		return "", fmt.Errorf("firmware.dir is not set, so this broker serves no firmware images to flash")
	}
	for _, img := range firmwareImages {
		if _, err := os.Stat(filepath.Join(dir, img.File)); err != nil {
			return "", fmt.Errorf("%s is missing from firmware.dir; copy the three build artifacts there after each firmware build", img.File)
		}
	}
	return dir, nil
}

// handleProvisionMint builds a fresh NVS provisioning image for one device and returns a
// short-lived manifest URL that ESP Web Tools can flash.
//
// The fleet Wi-Fi PSK never leaves the broker in a form a browser can read. An earlier sketch had
// the browser fetch the PSK and assemble the image client-side; that would make a shared,
// fleet-wide secret readable by anything running in an admin's tab.
//
// Each call mints a new token. There is deliberately no way to retrieve a device's existing
// token: the broker only stores the SHA-256, so reprovisioning a unit means reflashing it.
func (s *Server) handleProvisionMint(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("provision mint called with no fleet Wi-Fi configured", "device", req.DeviceID)
		http.Error(w, "fleet Wi-Fi credentials are not configured; set fleet.wifi_ssid and fleet.wifi_psk", http.StatusPreconditionFailed)
		return
	}
	if _, err := s.firmwareDirOrError(); err != nil {
		s.log.Error("provision mint blocked by firmware images", "device", req.DeviceID, "err", err)
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}

	// 32 bytes of entropy, base64url without padding — a full 256 bits, unlike
	// tools/provision_device.sh's strip-and-truncate scheme. The token is opaque to the firmware,
	// so both schemes coexist fine across a mixed fleet.
	tokenRaw := make([]byte, 32)
	nonceRaw := make([]byte, 32)
	if _, err := rand.Read(tokenRaw); err != nil {
		s.log.Error("provision token generation failed", "device", req.DeviceID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := rand.Read(nonceRaw); err != nil {
		s.log.Error("provision nonce generation failed", "device", req.DeviceID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenRaw)
	nonce := base64.RawURLEncoding.EncodeToString(nonceRaw)
	sum := sha256.Sum256([]byte(token))

	img, err := nvs.Build(nvsPartitionSize, "prov", []nvs.KV{
		{Key: "device_id", Value: req.DeviceID},
		{Key: "token", Value: token},
		{Key: "wifi_ssid", Value: cfg.Fleet.WifiSSID},
		{Key: "wifi_psk", Value: cfg.Fleet.WifiPSK},
	})
	if err != nil {
		s.log.Error("provision image build failed", "device", req.DeviceID, "err", err)
		http.Error(w, "could not build the provisioning image: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.provisions.put(nonce, &provisionArtifact{
		deviceID: req.DeviceID,
		nvs:      img,
		expires:  time.Now().Add(provisionTTL),
	})
	s.log.Info("provisioning image minted", "device", req.DeviceID, "expires_in", provisionTTL.String())

	writeJSONNoStore(w, s.log, provisionMintResponse{
		ManifestURL: "/admin/provision/" + nonce + "/manifest.json",
		TokenSHA256: hex.EncodeToString(sum[:]),
		ExpiresIn:   int(provisionTTL.Seconds()),
	})
}

// handleProvisionManifest serves the per-device ESP Web Tools manifest. Parts are listed in
// ascending flash offset; the NVS image is the only per-device one, and it resolves relative to
// this manifest's own URL.
func (s *Server) handleProvisionManifest(w http.ResponseWriter, r *http.Request) {
	nonce := r.PathValue("nonce")
	artifact, ok := s.provisions.get(nonce)
	if !ok {
		http.Error(w, "this provisioning link has expired; start the wizard again", http.StatusNotFound)
		return
	}
	if _, err := s.firmwareDirOrError(); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}

	parts := []espWebToolsPart{
		{Path: "/firmware/bootloader.bin", Offset: offsetBootloader},
		{Path: "/firmware/partition-table.bin", Offset: offsetPartitionTable},
		{Path: "nvs.bin", Offset: offsetNVS}, // relative: /admin/provision/<nonce>/nvs.bin
		{Path: "/firmware/meeting_display.bin", Offset: offsetApp},
	}

	// Version is whatever OTA is currently advertising; it's cosmetic here (ESP Web Tools shows it
	// in its dialog) but keeping it sourced from config means it can't drift into a hand-typed lie.
	version := s.cfg.Load().Firmware.Version
	if version == "" {
		version = "unversioned"
	}

	writeJSONNoStore(w, s.log, espWebToolsManifest{
		Name:    "Meeting Display (" + artifact.deviceID + ")",
		Version: version,
		// A re-flashed unit keeps its old otadata, which may point at ota_1 — writing only ota_0
		// would leave it booting the previous image. Erasing first also clears any stale NVS.
		NewInstallPromptErase: true,
		Builds: []espWebToolsBuild{{
			ChipFamily: "ESP32-S3",
			Parts:      parts,
			// The device gets its Wi-Fi from the NVS image, not from Improv over serial.
			Improv: false,
		}},
	})
}

// handleProvisionNVSImage serves the raw partition bytes for one minted device. Admin-gated like
// everything else under /admin, and no-store so the credential-bearing body can't be cached.
func (s *Server) handleProvisionNVSImage(w http.ResponseWriter, r *http.Request) {
	artifact, ok := s.provisions.get(r.PathValue("nonce"))
	if !ok {
		http.Error(w, "this provisioning link has expired; start the wizard again", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(artifact.nvs)
}

// handleFirmwareManifestPreflight reports whether the wizard can run at all, so the UI can
// explain a misconfiguration instead of failing at the moment someone clicks Install.
func (s *Server) handleProvisionPreflight(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	type preflight struct {
		Ready  bool     `json:"ready"`
		Issues []string `json:"issues"`
	}
	out := preflight{Issues: []string{}}
	if cfg.Fleet.WifiSSID == "" || cfg.Fleet.WifiPSK == "" {
		out.Issues = append(out.Issues, "Fleet Wi-Fi is not configured (set fleet.wifi_ssid and fleet.wifi_psk, with the PSK injected as MD_WIFI_PSK).")
	}
	if _, err := s.firmwareDirOrError(); err != nil {
		out.Issues = append(out.Issues, err.Error()+".")
	}
	out.Ready = len(out.Issues) == 0
	writeJSONNoStore(w, s.log, out)
}

func writeJSONNoStore(w http.ResponseWriter, log interface{ Error(string, ...any) }, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error("JSON response write failed", "err", err)
	}
}
