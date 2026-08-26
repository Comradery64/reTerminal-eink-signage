// Package configstore is where a live /admin or /manager edit goes to become durable: "file"
// (atomic temp-file + rename, via internal/config's FileWriter), "configmap" (today's in-cluster
// PATCH, via internal/kube's ConfigMapPersister), or "none" (ephemeral — a supported choice, not an
// error state). internal/config stays a leaf with no Kubernetes knowledge; this package is what
// wires config's file writer and kube's ConfigMap adapter behind one small interface so
// internal/server never needs to know which backend it's actually talking to.
package configstore

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/kube"
)

// Store is where a live /admin or /manager edit goes to become durable. Implementations must be
// atomic from a reader's point of view: after a crash mid-write, a reader sees either the whole
// old config or the whole new one, never a truncated file.
type Store interface {
	Persist(ctx context.Context, y []byte) error
	Mode() string   // "file" | "configmap" | "none" — the RESOLVED mode, never "auto"
	Target() string // human-readable destination for the UI, e.g. "/etc/meeting-displays/config.yaml"
	Durable() bool  // false only for "none"
}

// ConfigMapOptions carries the ConfigMap coordinates New needs. Duplicated here rather than
// configstore importing config.PersistConfigMapConfig directly, so this package's public surface
// doesn't force every caller (including tests that build Options by hand) to also import config's
// yaml-tagged type just to construct three strings.
type ConfigMapOptions struct{ Namespace, Name, Key string }

// Options configures New. Mode "" behaves as "auto" — see New's doc comment for the resolution
// order.
type Options struct {
	Mode       string // "", "auto", "file", "configmap", "none"
	ConfigPath string // the -config path; default target for the file backend
	FilePath   string // explicit file target override (config_persistence.file.path)
	ConfigMap  ConfigMapOptions
	Log        *slog.Logger
}

// newInClusterClient is kube.NewInClusterClient by default. Tests in this package reassign it to
// simulate being in-cluster (or not) without touching the real filesystem paths or environment
// variables the real probe reads.
var newInClusterClient = kube.NewInClusterClient

// New resolves opts.Mode and returns the Store to use for the rest of the process's life.
//
// "auto" (opts.Mode == "" or "auto") tries, in order:
//  1. in-cluster — kube.NewInClusterClient succeeding means this is today's production topology,
//     so the resolved mode ("configmap") reproduces today's behavior unchanged.
//  2. a writable file target.
//  3. "none", logged at ERROR — today's code took this path silently at Info, which is the exact
//     bug this whole change exists to kill: an operator who wanted durability got none of it, and
//     was never told.
//
// Any OTHER mode is an explicit operator choice and never falls back: a bad path or a missing
// in-cluster identity returns an error so main.go can exit non-zero, the same way a config.Load
// failure is already handled. Fail fast on misconfiguration; degrade only where degradation was
// actually requested (mode: none, or auto's own fallback).
func New(opts Options) (Store, error) {
	mode := opts.Mode
	if mode == "" {
		mode = "auto"
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	fileTarget := opts.FilePath
	if fileTarget == "" {
		fileTarget = opts.ConfigPath
	}

	switch mode {
	case "auto":
		if kc, err := newInClusterClient(); err == nil {
			return kube.NewConfigMapPersister(kc, opts.ConfigMap.Namespace, opts.ConfigMap.Name, opts.ConfigMap.Key), nil
		}
		if fw, err := config.NewFileWriter(fileTarget); err == nil {
			return &fileStore{fw: fw}, nil
		}
		log.Error("config persistence: not running in-cluster and the file target is not writable — admin/manager writes will NOT survive a restart; set config_persistence.mode explicitly to make this an informed choice instead of a silent one", "file_target", fileTarget)
		return noneStore{}, nil

	case "file":
		fw, err := config.NewFileWriter(fileTarget)
		if err != nil {
			return nil, fmt.Errorf("config_persistence.mode=file: %w", err)
		}
		return &fileStore{fw: fw}, nil

	case "configmap":
		kc, err := newInClusterClient()
		if err != nil {
			return nil, fmt.Errorf("config_persistence.mode=configmap: not running in-cluster: %w", err)
		}
		return kube.NewConfigMapPersister(kc, opts.ConfigMap.Namespace, opts.ConfigMap.Name, opts.ConfigMap.Key), nil

	case "none":
		return noneStore{}, nil

	default:
		return nil, fmt.Errorf("config_persistence.mode must be 'auto', 'file', 'configmap', or 'none', got %q", mode)
	}
}

// fileStore adapts config.FileWriter to Store.
type fileStore struct{ fw *config.FileWriter }

func (f *fileStore) Persist(_ context.Context, y []byte) error { return f.fw.WriteAtomic(y) }
func (f *fileStore) Mode() string                              { return "file" }
func (f *fileStore) Target() string                            { return f.fw.Path() }
func (f *fileStore) Durable() bool                             { return true }

// noneStore is the explicit "don't persist" choice — a supported option (demo, evaluation,
// immutable-config deployments), not an error state. Persist is a genuine no-op: the caller's
// write still applies in memory (see server.applyConfig), it just never reaches disk.
type noneStore struct{}

func (noneStore) Persist(context.Context, []byte) error { return nil }
func (noneStore) Mode() string                          { return "none" }
func (noneStore) Target() string                        { return "" }
func (noneStore) Durable() bool                         { return false }
