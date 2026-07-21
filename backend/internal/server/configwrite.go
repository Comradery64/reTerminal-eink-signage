package server

import (
	"context"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// These must match the ConfigMap the broker's Deployment mounts config.yaml from (see
// deploy/k3s/broker.yaml.example) — they're deployment-topology constants, not something an
// admin/manager write could reasonably need to vary per-request.
const (
	kubeNamespace     = "meeting-displays"
	kubeConfigMapName = "broker-config"
	kubeConfigMapKey  = "config.yaml"
)

// applyConfig is the single point every admin/manager write goes through after producing an
// already-Validate()'d *config.Config (via config.WithRoom / WithoutRoom / WithRoomWakeOverride):
// it swaps the in-memory config synchronously, so every subsequent request sees the change
// immediately, then asynchronously persists it to the ConfigMap — mirroring notify.Manager's
// "mutate under lock, dispatch async after releasing it" idiom. refreshCaches must be true for
// any write that could affect device tokens, room names, or login credentials (room add/edit/
// delete, credential change); wake-only writes can skip it.
func (s *Server) applyConfig(newCfg *config.Config, refreshCaches bool) {
	s.cfg.Store(newCfg)
	if refreshCaches {
		s.refreshDerived(newCfg)
	}
	if s.kube == nil {
		s.log.Warn("no in-cluster kube client — config change is live in-memory only and will NOT survive a restart")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := yaml.Marshal(newCfg)
		if err != nil {
			s.log.Error("marshal config for ConfigMap patch failed", "err", err)
			return
		}
		if err := s.kube.PatchConfigMapKey(ctx, kubeNamespace, kubeConfigMapName, kubeConfigMapKey, out); err != nil {
			s.log.Error("configmap patch failed — change is live in-memory but WON'T survive a restart", "err", err)
		}
	}()
}
