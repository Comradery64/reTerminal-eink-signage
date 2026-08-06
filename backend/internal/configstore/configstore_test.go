package configstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/kube"
)

// withInClusterProbe overrides the package's in-cluster probe for the duration of the test,
// restoring it afterward — this is how the resolver's "auto" branch gets table-tested without
// touching real KUBERNETES_SERVICE_HOST/token-mount state.
func withInClusterProbe(t *testing.T, fn func() (*kube.Client, error)) {
	t.Helper()
	orig := newInClusterClient
	newInClusterClient = fn
	t.Cleanup(func() { newInClusterClient = orig })
}

func TestResolveAutoPrefersInCluster(t *testing.T) {
	withInClusterProbe(t, func() (*kube.Client, error) { return &kube.Client{}, nil })

	st, err := New(Options{
		Mode:       "auto",
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		ConfigMap:  ConfigMapOptions{Namespace: "meeting-displays", Name: "broker-config", Key: "config.yaml"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if st.Mode() != "configmap" {
		t.Fatalf("Mode() = %q, want configmap (in-cluster must win even though the file target is also writable)", st.Mode())
	}
	if !st.Durable() {
		t.Fatal("configmap must be durable")
	}
}

func TestResolveAutoFallsBackToFileWhenNotInCluster(t *testing.T) {
	withInClusterProbe(t, func() (*kube.Client, error) { return nil, errors.New("not in cluster") })

	path := filepath.Join(t.TempDir(), "config.yaml")
	st, err := New(Options{Mode: "auto", ConfigPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if st.Mode() != "file" {
		t.Fatalf("Mode() = %q, want file", st.Mode())
	}
	if st.Target() != path {
		t.Fatalf("Target() = %q, want %q", st.Target(), path)
	}
	if !st.Durable() {
		t.Fatal("file must be durable")
	}
}

func TestResolveAutoFallsBackToNoneWhenNeitherAvailable(t *testing.T) {
	withInClusterProbe(t, func() (*kube.Client, error) { return nil, errors.New("not in cluster") })

	st, err := New(Options{Mode: "auto", ConfigPath: filepath.Join("/nonexistent-dir-configstore-test", "config.yaml")})
	if err != nil {
		t.Fatalf("auto must never itself error — it falls back to none: %v", err)
	}
	if st.Mode() != "none" {
		t.Fatalf("Mode() = %q, want none", st.Mode())
	}
	if st.Durable() {
		t.Fatal("none must never be durable")
	}
}

func TestResolveExplicitFileFailsClosedOnBadPath(t *testing.T) {
	_, err := New(Options{Mode: "file", ConfigPath: filepath.Join("/nonexistent-dir-configstore-test", "config.yaml")})
	if err == nil {
		t.Fatal("an explicit file mode with an unwritable path must return an error, not silently fall back")
	}
}

func TestResolveExplicitConfigMapFailsClosedOutsideCluster(t *testing.T) {
	withInClusterProbe(t, func() (*kube.Client, error) { return nil, errors.New("not in cluster") })

	if _, err := New(Options{Mode: "configmap"}); err == nil {
		t.Fatal("an explicit configmap mode outside a cluster must return an error, not silently fall back")
	}
}

func TestResolveNoneIsAlwaysAvailableAndPersistIsANoOp(t *testing.T) {
	st, err := New(Options{Mode: "none"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if st.Durable() {
		t.Fatal("none must never be durable")
	}
	if err := st.Persist(context.Background(), []byte("x")); err != nil {
		t.Fatalf("none.Persist should always succeed as a no-op, got %v", err)
	}
}

func TestResolveRejectsUnknownMode(t *testing.T) {
	if _, err := New(Options{Mode: "bogus"}); err == nil {
		t.Fatal("want an error for an unknown mode")
	}
}

func TestResolveFileUsesExplicitFilePathOverConfigPath(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "explicit.yaml")
	configPath := filepath.Join(dir, "config.yaml")

	st, err := New(Options{Mode: "file", ConfigPath: configPath, FilePath: explicit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if st.Target() != explicit {
		t.Fatalf("Target() = %q, want the explicit file.path override %q", st.Target(), explicit)
	}

	if err := st.Persist(context.Background(), []byte("listen: :8080\n")); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	got, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "listen: :8080\n" {
		t.Fatalf("file content = %q", got)
	}
}
