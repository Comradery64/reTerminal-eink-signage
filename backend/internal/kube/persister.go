package kube

import (
	"context"
	"fmt"
)

// ConfigMapPersister adapts Client.PatchConfigMapKey to the small (Persist/Mode/Target/Durable)
// shape internal/configstore.Store expects — structurally, not by import: kube must not import
// configstore or config, since either would recreate the exact in-cluster coupling this whole
// change exists to remove from every package except this one adapter.
type ConfigMapPersister struct {
	client *Client

	Namespace string
	Name      string
	Key       string
}

// NewConfigMapPersister builds a ConfigMapPersister over an already-constructed in-cluster Client.
// namespace/name/key used to be hardcoded constants in internal/server/configwrite.go — they're
// fields now so config_persistence.configmap.{namespace,name,key} can override them, with the
// same values as defaults (see config.applyDefaults).
func NewConfigMapPersister(client *Client, namespace, name, key string) *ConfigMapPersister {
	return &ConfigMapPersister{client: client, Namespace: namespace, Name: name, Key: key}
}

func (p *ConfigMapPersister) Persist(ctx context.Context, y []byte) error {
	return p.client.PatchConfigMapKey(ctx, p.Namespace, p.Name, p.Key, y)
}

func (p *ConfigMapPersister) Mode() string { return "configmap" }

func (p *ConfigMapPersister) Target() string { return fmt.Sprintf("%s/%s", p.Namespace, p.Name) }

// Durable is always true: a successful Persist means the API server has already accepted and
// stored the PATCH — there's no further durability question the way there is for a Sink queueing a
// write to be flushed later.
func (p *ConfigMapPersister) Durable() bool { return true }
