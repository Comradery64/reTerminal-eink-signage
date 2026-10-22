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
		// The k3s initial config. It is the highest-stakes of these: when wrong it takes the fleet
		// down rather than annoying a reader.
		{"config.seed.yaml.example", filepath.Join("..", "..", "deploy", "k3s", "config.seed.yaml.example")},
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

	// The live config ConfigMap must never be in the manifest an operator applies. The broker edits
	// it itself from /admin (users, 2FA secrets, rooms, ...), so applying a copy from git replaces
	// all of that — it did, and accounts lost their 2FA. The initial config is config.seed.yaml.example,
	// created once with `kubectl create`. A guard that cannot fail guards nothing: the second
	// subtest proves the detector does trip on a manifest that carries it.
	t.Run("broker.yaml.example does not carry the live config ConfigMap", func(t *testing.T) {
		if manifestHasBrokerConfigMap(t, filepath.Join("..", "..", "deploy", "k3s", "broker.yaml.example")) {
			t.Fatal("broker.yaml.example contains the broker-config ConfigMap: `kubectl apply` of it would overwrite live users, 2FA secrets and rooms. Keep the config in config.seed.yaml.example (first install only).")
		}
	})
	t.Run("guard trips on a manifest that carries it", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "manifest.yaml")
		yml := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: broker-config\ndata:\n  config.yaml: |\n    listen: \":8080\"\n"
		if err := os.WriteFile(bad, []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		if !manifestHasBrokerConfigMap(t, bad) {
			t.Fatal("manifestHasBrokerConfigMap did not flag a manifest containing broker-config")
		}
	})
}

// manifestHasBrokerConfigMap reports whether a multi-document k8s manifest defines the ConfigMap
// named broker-config.
func manifestHasBrokerConfigMap(t *testing.T, manifest string) bool {
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
		}
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return false
		} else if err != nil {
			t.Fatal(err)
		}
		if doc.Kind == "ConfigMap" && doc.Metadata.Name == "broker-config" {
			return true
		}
	}
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
