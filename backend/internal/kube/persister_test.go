package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestConfigMapPersisterPersistSendsExpectedPatch(t *testing.T) {
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	})
	p := NewConfigMapPersister(c, "meeting-displays", "broker-config", "config.yaml")

	if err := p.Persist(context.Background(), []byte("listen: :9090\n")); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	data, _ := gotBody["data"].(map[string]any)
	if data["config.yaml"] != "listen: :9090\n" {
		t.Errorf("patch body data.config.yaml = %v", data["config.yaml"])
	}
}

func TestConfigMapPersisterModeTargetDurable(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	p := NewConfigMapPersister(c, "meeting-displays", "broker-config", "config.yaml")

	if p.Mode() != "configmap" {
		t.Errorf("Mode() = %q, want configmap", p.Mode())
	}
	if p.Target() != "meeting-displays/broker-config" {
		t.Errorf("Target() = %q, want meeting-displays/broker-config", p.Target())
	}
	if !p.Durable() {
		t.Error("Durable() should always be true for the configmap backend")
	}
}

func TestConfigMapPersisterPersistPropagatesServerError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	})
	p := NewConfigMapPersister(c, "meeting-displays", "broker-config", "config.yaml")

	if err := p.Persist(context.Background(), []byte("x")); err == nil {
		t.Fatal("want an error on a non-200 response")
	}
}
