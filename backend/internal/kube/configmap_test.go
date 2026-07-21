package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("fake-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Client{httpClient: srv.Client(), apiServer: srv.URL, tokenPath: tokenFile}
}

func TestPatchConfigMapKeySendsExpectedRequest(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody map[string]any

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	})

	if err := c.PatchConfigMapKey(context.Background(), "meeting-displays", "broker-config", "config.yaml", []byte("listen: :8080\n")); err != nil {
		t.Fatalf("PatchConfigMapKey: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/api/v1/namespaces/meeting-displays/configmaps/broker-config" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer fake-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer fake-token")
	}
	if gotContentType != "application/merge-patch+json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	data, _ := gotBody["data"].(map[string]any)
	if data["config.yaml"] != "listen: :8080\n" {
		t.Errorf("patch body data.config.yaml = %v", data["config.yaml"])
	}
}

func TestPatchConfigMapKeyPropagatesServerError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	})

	if err := c.PatchConfigMapKey(context.Background(), "meeting-displays", "broker-config", "config.yaml", nil); err == nil {
		t.Fatal("want an error on non-200 response")
	}
}
