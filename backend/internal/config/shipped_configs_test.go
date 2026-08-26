package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file exists because the same bug shipped twice in one day, at two different layers.
//
// Load was tightened to reject a ${VAR} whose environment variable is unset (see
// TestLoadRejectsDanglingEnvRef). Both times the rule was correct and the fallout was in artifacts
// the repo ships that must satisfy it:
//
//  1. config.example.yaml documents the ${VAR} mechanism in its own YAML comments, so the first
//     implementation made a config unstartable because of its own documentation.
//  2. config.tier1.example.yaml and the ConfigMap embedded in broker.yaml.example referenced
//     ${ALERT_WEBHOOK_URL} and ${MD_WIFI_PSK} unconditionally, while the deploy docs (and the
//     Deployment's own `optional: true`) tell operators those keys may be omitted. Following the
//     documentation produced a broker that refused to start — on k3s, a crashloop on a value the
//     docs called optional.
//
// A green unit-test suite caught neither: both needed the shipped artifact actually loaded. So this
// test loads every config the repo ships, with only the genuinely-required variable set. Any future
// change to Load or Validate that breaks a fresh install fails here instead of in someone's cluster.
func TestShippedConfigsLoadWithOptionalEnvUnset(t *testing.T) {
	// SESSION_SECRET is required whenever users exist (>=32 bytes), so it has no valid omitted
	// case and a shipped example may legitimately reference it. Everything else must survive being
	// unset, because the docs say those keys are optional.
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 40))
	os.Unsetenv("ALERT_WEBHOOK_URL")
	os.Unsetenv("MD_WIFI_PSK")

	for _, tc := range []struct {
		name string
		path string
	}{
		{"config.example.yaml", filepath.Join("..", "..", "config.example.yaml")},
		{"config.demo.yaml", filepath.Join("..", "..", "config.demo.yaml")},
		{"config.tier1.example.yaml", filepath.Join("..", "..", "config.tier1.example.yaml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNoDanglingEnvRef(t, tc.path)
		})
	}

	// A guard that cannot fail guards nothing. This proves assertNoDanglingEnvRef actually trips on
	// the condition it exists to catch, so a future refactor that breaks the detection (say, by
	// changing Load's error text) shows up as a failure here rather than as four silently
	// meaningless PASSes.
	t.Run("guard trips on a dangling ref", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "config.yaml")
		yml := "listen: \":8080\"\nprovider: \"demo\"\nalerts:\n  webhook_url: \"${ALERT_WEBHOOK_URL}\"\n"
		if err := os.WriteFile(bad, []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		if danglingEnvRefErr(bad) == nil {
			t.Fatal("danglingEnvRefErr did not flag a config with an unset ${VAR} — the guard is " +
				"inert and the four cases below prove nothing")
		}
	})

	// The k3s ConfigMap is the highest-stakes of the four: it is the one that, when wrong, takes
	// the fleet down rather than annoying a reader. It is embedded inside a multi-document
	// manifest, so it has to be extracted before it can be loaded.
	t.Run("broker.yaml.example ConfigMap", func(t *testing.T) {
		embedded := extractConfigMapYAML(t, filepath.Join("..", "..", "deploy", "k3s", "broker.yaml.example"))
		tmp := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(tmp, []byte(embedded), 0o600); err != nil {
			t.Fatal(err)
		}
		assertNoDanglingEnvRef(t, tmp)
	})
}

// assertNoDanglingEnvRef fails only on the env-reference error. Every shipped example still carries
// REPLACE_-style room placeholders and is expected to fail validation on those — that is the
// template working as intended (see the README quick start note), not a regression.
func assertNoDanglingEnvRef(t *testing.T, path string) {
	t.Helper()
	if err := danglingEnvRefErr(path); err != nil {
		t.Fatalf("%s does not start with the optional env vars unset — a fresh install following "+
			"the docs would fail to boot: %v", path, err)
	}
	t.Logf("%s: no dangling env refs", path)
}

// danglingEnvRefErr returns non-nil only when path fails to load *because of* an unset ${VAR}.
// Split out from the assertion so the guard-trips subtest can exercise it as a plain function —
// driving a synthetic *testing.T instead would hit runtime.Goexit outside a test goroutine.
func danglingEnvRefErr(path string) error {
	_, err := Load(path)
	if err != nil && strings.Contains(err.Error(), "references environment variable") {
		return err
	}
	return nil // nil, or a placeholder-validation error, which is the template working as intended
}

// extractConfigMapYAML pulls data["config.yaml"] out of the broker-config ConfigMap in a
// multi-document k8s manifest.
func extractConfigMapYAML(t *testing.T, manifest string) string {
	t.Helper()
	f, err := os.Open(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Data     map[string]string     `yaml:"data"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			continue // manifests may hold documents that don't fit this shape; skip them
		}
		if doc.Kind == "ConfigMap" && doc.Data != nil {
			if cfg, ok := doc.Data["config.yaml"]; ok {
				return cfg
			}
		}
	}
	t.Fatalf("no ConfigMap with data[\"config.yaml\"] found in %s — if the manifest was "+
		"restructured, update this test rather than deleting it", manifest)
	return ""
}
