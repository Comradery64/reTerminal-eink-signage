package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// provisionServer is testServerWithAuth plus the two preconditions the wizard needs: fleet Wi-Fi
// credentials, and a firmware.dir holding the three build artifacts.
func provisionServer(t *testing.T) *Server {
	t.Helper()
	s := testServerWithAuth(t)
	dir := t.TempDir()
	for _, img := range firmwareImages {
		if err := os.WriteFile(filepath.Join(dir, img.File), []byte("fake image"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := *s.cfg.Load()
	cfg.Fleet = config.FleetConfig{WifiSSID: "Example_Fleet_WiFi", WifiPSK: "test-psk-value"}
	cfg.Firmware.Dir = dir
	cfg.Firmware.Version = "1.0.0"
	s.cfg.Store(&cfg)
	return s
}

func mint(t *testing.T, client *http.Client, base, body string) *http.Response {
	t.Helper()
	resp, err := client.Post(base+"/admin/api/provision-nvs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mintOK(t *testing.T, client *http.Client, base, deviceID string) provisionMintResponse {
	t.Helper()
	resp := mint(t, client, base, `{"device_id":"`+deviceID+`"}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint: want 200, got %d: %s", resp.StatusCode, b)
	}
	var got provisionMintResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestProvisionRequiresAdmin(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	for _, path := range []string{"/admin/api/provision-preflight", "/admin/provision/anything/manifest.json", "/admin/provision/anything/nvs.bin"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s is reachable without an admin session", path)
		}
	}
	if resp := mint(t, client, srv.URL, `{"device_id":"rt-9"}`); resp.StatusCode == http.StatusOK {
		t.Error("minting is reachable without an admin session")
	}
}

// The mint response must not carry the plaintext token or the fleet PSK — that is the whole point
// of assembling the image server-side.
func TestProvisionMintLeaksNoCredentialsToTheBrowser(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp := mint(t, client, srv.URL, `{"device_id":"rt-9"}`)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, raw)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	if bytes.Contains(raw, []byte("test-psk-value")) {
		t.Error("mint response contains the fleet Wi-Fi PSK")
	}
	if bytes.Contains(raw, []byte("nvs_b64")) {
		t.Error("mint response still ships raw image bytes to the browser")
	}

	var got provisionMintResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !isSHA256Hex(got.TokenSHA256) {
		t.Errorf("token_sha256 = %q, want 64 lowercase hex chars", got.TokenSHA256)
	}
	if !strings.HasPrefix(got.ManifestURL, "/admin/provision/") {
		t.Errorf("manifest_url = %q, want an /admin/provision/ path", got.ManifestURL)
	}
}

// The manifest has to describe a flash layout that matches firmware/partitions.csv, in ascending
// offset, or ESP Web Tools writes the images to the wrong places and the unit won't boot.
func TestProvisionManifestMatchesPartitionLayout(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	minted := mintOK(t, client, srv.URL, "rt-9")
	resp, err := client.Get(srv.URL + minted.ManifestURL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest: want 200, got %d", resp.StatusCode)
	}
	var m espWebToolsManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}

	if len(m.Builds) != 1 || m.Builds[0].ChipFamily != "ESP32-S3" {
		t.Fatalf("want exactly one ESP32-S3 build, got %+v", m.Builds)
	}
	if !m.NewInstallPromptErase {
		t.Error("new_install_prompt_erase must be true, or a re-flashed unit keeps stale otadata and boots the old image")
	}
	// Offsets mirror firmware/partitions.csv exactly, and the set mirrors the flash command
	// `idf.py build` prints. ota_data_initial.bin at 0x10000 is load-bearing — see firmwareImages.
	want := []espWebToolsPart{
		{Path: "/firmware/bootloader.bin", Offset: 0x0},
		{Path: "/firmware/partition-table.bin", Offset: 0x8000},
		{Path: "nvs.bin", Offset: 0x9000},
		{Path: "/firmware/ota_data_initial.bin", Offset: 0x10000},
		{Path: "/firmware/meeting_display.bin", Offset: 0x20000},
	}
	got := m.Builds[0].Parts
	if len(got) != len(want) {
		t.Fatalf("want %d parts, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Offset <= got[i-1].Offset {
			t.Errorf("parts must ascend by offset: part %d (%#x) follows %#x", i, got[i].Offset, got[i-1].Offset)
		}
	}
}

// The flashed image must contain a token that hashes to what the wizard stores against the room —
// if these disagreed, every provisioned device would fail auth on its first check-in.
func TestProvisionImageTokenMatchesReturnedHash(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	minted := mintOK(t, client, srv.URL, "rt-9")
	nonce := strings.Split(strings.TrimPrefix(minted.ManifestURL, "/admin/provision/"), "/")[0]

	resp, err := client.Get(srv.URL + "/admin/provision/" + nonce + "/nvs.bin")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nvs.bin: want 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("nvs.bin Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	img, _ := io.ReadAll(resp.Body)
	if len(img) != nvsPartitionSize {
		t.Fatalf("image is %d bytes, want %d", len(img), nvsPartitionSize)
	}
	for _, want := range []string{"prov", "device_id", "rt-9", "wifi_ssid", "Example_Fleet_WiFi", "wifi_psk", "test-psk-value"} {
		if !bytes.Contains(img, []byte(want)) {
			t.Errorf("image is missing %q", want)
		}
	}

	// Find the token by hashing every 43-byte window rather than reimplementing an NVS reader.
	const tokenLen = 43
	found := false
	for i := 0; i+tokenLen <= len(img); i++ {
		sum := sha256.Sum256(img[i : i+tokenLen])
		if hex.EncodeToString(sum[:]) == minted.TokenSHA256 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no token in the image hashes to the returned token_sha256")
	}
}

func TestProvisionMintsAFreshTokenEachCall(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	a := mintOK(t, client, srv.URL, "rt-9")
	b := mintOK(t, client, srv.URL, "rt-9")
	if a.TokenSHA256 == b.TokenSHA256 {
		t.Error("two mints produced the same token; each flash must get a fresh one")
	}
	if a.ManifestURL == b.ManifestURL {
		t.Error("two mints produced the same manifest URL; nonces must be unguessable and distinct")
	}
}

func TestProvisionRejectsUnknownNonce(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	for _, path := range []string{"/admin/provision/never-minted/manifest.json", "/admin/provision/never-minted/nvs.bin"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", path, resp.StatusCode)
		}
	}
}

// An expired artifact must stop being fetchable, since its URL carries a live credential.
func TestProvisionArtifactExpires(t *testing.T) {
	store := newProvisionStore()
	store.put("fresh", &provisionArtifact{deviceID: "rt-9", nvs: []byte{1}, expires: time.Now().Add(time.Minute)})
	store.put("stale", &provisionArtifact{deviceID: "rt-9", nvs: []byte{1}, expires: time.Now().Add(-time.Minute)})

	if _, ok := store.get("fresh"); !ok {
		t.Error("an unexpired artifact should still be fetchable")
	}
	if _, ok := store.get("stale"); ok {
		t.Error("an expired artifact must not be fetchable")
	}
}

func TestProvisionPreflightReportsMissingPreconditions(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*config.Config)
		wantReady bool
		wantIssue string
	}{
		{"ready", func(*config.Config) {}, true, ""},
		{"no fleet wifi", func(c *config.Config) { c.Fleet = config.FleetConfig{} }, false, "Fleet Wi-Fi"},
		{"no firmware dir", func(c *config.Config) { c.Firmware.Dir = "" }, false, "firmware.dir"},
		{"firmware dir missing images", func(c *config.Config) { c.Firmware.Dir = os.TempDir() }, false, "bootloader.bin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := provisionServer(t)
			cfg := *s.cfg.Load()
			tc.mutate(&cfg)
			s.cfg.Store(&cfg)

			srv := httptest.NewTLSServer(s.Handler())
			defer srv.Close()
			client := loggedInAdminClient(t, srv)

			resp, err := client.Get(srv.URL + "/admin/api/provision-preflight")
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Ready  bool     `json:"ready"`
				Issues []string `json:"issues"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Ready != tc.wantReady {
				t.Fatalf("ready = %v, want %v (issues: %v)", got.Ready, tc.wantReady, got.Issues)
			}
			if tc.wantIssue != "" && !strings.Contains(strings.Join(got.Issues, " "), tc.wantIssue) {
				t.Errorf("issues %v should mention %q", got.Issues, tc.wantIssue)
			}
		})
	}
}

// Minting must refuse when a precondition is unmet, rather than handing back an image that would
// flash a unit which silently never joins the network.
func TestProvisionMintRefusesWhenPreconditionsUnmet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"no fleet wifi", func(c *config.Config) { c.Fleet = config.FleetConfig{} }},
		{"no firmware dir", func(c *config.Config) { c.Firmware.Dir = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := provisionServer(t)
			cfg := *s.cfg.Load()
			tc.mutate(&cfg)
			s.cfg.Store(&cfg)

			srv := httptest.NewTLSServer(s.Handler())
			defer srv.Close()
			client := loggedInAdminClient(t, srv)

			if resp := mint(t, client, srv.URL, `{"device_id":"rt-9"}`); resp.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("want 412, got %d", resp.StatusCode)
			}
		})
	}
}

func TestProvisionMintRejectsBadRequests(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	for _, body := range []string{`{}`, `{"device_id":""}`, `not json`} {
		if resp := mint(t, client, srv.URL, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: want 400, got %d", body, resp.StatusCode)
		}
	}
}
