package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// provisionServer is testServerWithAuth plus fleet Wi-Fi credentials, which the endpoint requires.
func provisionServer(t *testing.T) *Server {
	t.Helper()
	s := testServerWithAuth(t)
	cfg := *s.cfg.Load()
	cfg.Fleet = config.FleetConfig{WifiSSID: "Test_SSID", WifiPSK: "test-psk-value"}
	s.cfg.Store(&cfg)
	return s
}

func postProvision(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	resp, err := client.Post(url+"/admin/api/provision-nvs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProvisionNVSRequiresAdmin(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp := postProvision(t, client, srv.URL, `{"device_id":"rt-9"}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("provisioning must not be reachable without an admin session")
	}
}

func TestProvisionNVSReturnsFlashableImageAndHash(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp := postProvision(t, client, srv.URL, `{"device_id":"rt-9"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the body carries the token and the fleet PSK", cc)
	}

	var got provisionNVSResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.NVSOffset != 0x9000 {
		t.Errorf("nvs_offset = %#x, want 0x9000 (firmware/partitions.csv)", got.NVSOffset)
	}
	if !isSHA256Hex(got.TokenSHA256) {
		t.Errorf("token_sha256 = %q, want 64 lowercase hex chars", got.TokenSHA256)
	}

	img, err := base64.StdEncoding.DecodeString(got.NVSBase64)
	if err != nil {
		t.Fatalf("nvs_b64 is not valid base64: %v", err)
	}
	if len(img) != nvsPartitionSize {
		t.Fatalf("image is %d bytes, want %d", len(img), nvsPartitionSize)
	}
	// The image must actually carry what the firmware reads back out (see nvs_store.cpp). Checking
	// for the raw strings is crude but catches a wrong namespace, a dropped key, or empty config
	// far more directly than re-parsing the NVS format here.
	for _, want := range []string{"prov", "device_id", "rt-9", "wifi_ssid", "Test_SSID", "wifi_psk", "test-psk-value", "token"} {
		if !bytes.Contains(img, []byte(want)) {
			t.Errorf("provisioning image is missing %q", want)
		}
	}
}

// The plaintext token must appear in the flashed image and hash to the value the wizard will
// store — if these ever disagreed, every provisioned device would fail auth on first check-in.
func TestProvisionNVSTokenHashMatchesEmbeddedToken(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp := postProvision(t, client, srv.URL, `{"device_id":"rt-9"}`)
	var got provisionNVSResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	img, _ := base64.StdEncoding.DecodeString(got.NVSBase64)

	// The token is a 43-char base64url string stored NUL-terminated right after its entry header;
	// find it by hashing every 43-byte window rather than reimplementing an NVS reader.
	const tokenLen = 43
	found := false
	for i := 0; i+tokenLen <= len(img); i++ {
		sum := sha256.Sum256(img[i : i+tokenLen])
		if hex.EncodeToString(sum[:]) == got.TokenSHA256 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no token in the image hashes to the returned token_sha256")
	}
}

func TestProvisionNVSMintsAFreshTokenEachCall(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	var hashes []string
	for i := 0; i < 2; i++ {
		resp := postProvision(t, client, srv.URL, `{"device_id":"rt-9"}`)
		var got provisionNVSResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, got.TokenSHA256)
	}
	if hashes[0] == hashes[1] {
		t.Fatal("two provisioning calls produced the same token; each flash must get a fresh one")
	}
}

// Without fleet Wi-Fi configured the endpoint must refuse rather than hand back an image that
// would flash a unit which silently never joins the network.
func TestProvisionNVSRefusesWithoutFleetWifi(t *testing.T) {
	s := testServerWithAuth(t) // deliberately no Fleet config
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	resp := postProvision(t, client, srv.URL, `{"device_id":"rt-9"}`)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("want 412, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "fleet.wifi_ssid") {
		t.Errorf("error should name the config keys to set, got %q", body)
	}
}

func TestProvisionNVSRejectsBadRequests(t *testing.T) {
	s := provisionServer(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	client := loggedInAdminClient(t, srv)

	for _, body := range []string{`{}`, `{"device_id":""}`, `not json`} {
		resp := postProvision(t, client, srv.URL, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: want 400, got %d", body, resp.StatusCode)
		}
	}
}
