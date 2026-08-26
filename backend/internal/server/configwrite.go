package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// persistTimeout bounds how long a single admin/manager write waits on the durable persist step —
// generous for a sub-second rename or ConfigMap PATCH, since these are rare human-driven writes,
// not device traffic on a battery budget.
const persistTimeout = 5 * time.Second

// persistState is the standing, mutex-guarded summary of the persistence backend's health. It
// backs the strip rendered on every /admin and /manager page load (see persistStrip below) and the
// machine-readable GET /api/v1/config-persistence — a one-shot flash banner is missable, and
// invisible to the other admin who logs in an hour later after a write silently failed.
type persistState struct {
	mu sync.RWMutex

	mode      string
	target    string
	durable   bool
	lastErr   string
	lastErrAt time.Time
	lastOKAt  time.Time
}

type persistStateView struct {
	Mode      string
	Target    string
	Durable   bool
	LastErr   string
	LastErrAt time.Time
	LastOKAt  time.Time
}

func (p *persistState) snapshot() persistStateView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return persistStateView{
		Mode: p.mode, Target: p.target, Durable: p.durable,
		LastErr: p.lastErr, LastErrAt: p.lastErrAt, LastOKAt: p.lastOKAt,
	}
}

func (p *persistState) recordOK(mode, target string, durable bool, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode, p.target, p.durable = mode, target, durable
	p.lastErr = ""
	p.lastOKAt = at
}

func (p *persistState) recordErr(mode, target string, durable bool, err error, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode, p.target, p.durable = mode, target, durable
	p.lastErr = err.Error()
	p.lastErrAt = at
}

// applyConfig is the single point every admin/manager write goes through after producing an
// already-Validate()'d *config.Config (via config.WithRoom / WithoutRoom / WithRoomWakeOverride):
// it PERSISTS FIRST and only swaps the live config in if the write was durable, so a human who is
// told "Saved" has actually been told the truth. Fail-closed is consistent with the rest of the
// write path: WithRoom/WithUser already refuse an invalid edit rather than applying it with a
// warning.
//
// writeMu is held across marshal -> persist -> cfg.Store -> refreshDerived, which also removes
// today's real race where two concurrent admin saves could land in memory and in the ConfigMap in
// opposite orders. A disconnected browser must not abort a write already in flight, hence
// WithoutCancel — persistTimeout is a generous budget for a sub-second rename or PATCH.
func (s *Server) applyConfig(ctx context.Context, newCfg *config.Config, refreshCaches bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	mode, target, durable := s.persist.Mode(), s.persist.Target(), s.persist.Durable()

	if durable {
		out, err := newCfg.MarshalForPersist()
		if err != nil {
			wrapped := fmt.Errorf("could not marshal config for %s: %w — nothing was changed", target, err)
			s.pstate.recordErr(mode, target, durable, wrapped, time.Now())
			return wrapped
		}

		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
		defer cancel()
		if err := s.persist.Persist(pctx, out); err != nil {
			wrapped := persistFailureMessage(mode, target, err)
			s.pstate.recordErr(mode, target, durable, wrapped, time.Now())
			return wrapped
		}
	}

	s.cfg.Store(newCfg)
	if refreshCaches {
		s.refreshDerived(newCfg)
	}
	s.pstate.recordOK(mode, target, durable, time.Now())
	return nil
}

// persistFailureMessage is written for the human reading the /admin red banner: it names the
// destination and the fix, not just the raw driver error — reusing the existing banner, no new UI
// vocabulary.
func persistFailureMessage(mode, target string, err error) error {
	switch mode {
	case "configmap":
		return fmt.Errorf("could not save to ConfigMap %s: %v — nothing was changed. Check the broker ServiceAccount's RBAC (docs/DEPLOY-TIERS.md#tier-3-403-triage-checklist), or start the broker with -config-persistence=none for ephemeral changes", target, err)
	case "file":
		return fmt.Errorf("could not save to %s: %v — nothing was changed. Check the path is writable, or start the broker with -config-persistence=none for ephemeral changes", target, err)
	default:
		return fmt.Errorf("could not save config (%s): %v — nothing was changed", mode, err)
	}
}

// persistStripView is what /admin and /manager render as the standing persistence strip on every
// page load — a one-shot flash banner is missable, and invisible to the other admin who logs in an
// hour later after a write silently failed.
type persistStripView struct {
	Class   string // "" (healthy) | "banner-warn" (none) | "banner-error" (last write failed)
	Message string
}

func (s *Server) persistStrip() persistStripView {
	v := s.pstate.snapshot()
	if v.LastErr != "" {
		return persistStripView{
			Class:   "banner-error",
			Message: fmt.Sprintf("LAST SAVE FAILED %s — %s", roundedAgo(v.LastErrAt), v.LastErr),
		}
	}
	if !v.Durable {
		return persistStripView{
			Class:   "banner-warn",
			Message: "Persistence: none — changes take effect immediately but are LOST when the broker restarts.",
		}
	}
	dest := v.Target
	switch v.Mode {
	case "configmap":
		dest = "ConfigMap " + v.Target
	case "file":
		dest = "file " + v.Target
	}
	return persistStripView{Message: "Saving to " + dest}
}

// roundedAgo renders t as a coarse "Nm ago" / "just now" — precise enough for a status strip a
// human glances at, without the sub-second noise of time.Duration's default String().
func roundedAgo(t time.Time) string {
	if t.IsZero() {
		return "just now"
	}
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	return fmt.Sprintf("%dm ago", int(d/time.Minute))
}

// handleConfigPersistenceJSON is the machine-readable twin of the persistence strip — the hook for
// a Tier 3 blackbox probe or a Tier 1 cron to alert on, rather than scraping the /admin HTML.
func (s *Server) handleConfigPersistenceJSON(w http.ResponseWriter, r *http.Request) {
	v := s.pstate.snapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Mode        string `json:"mode"`
		Target      string `json:"target"`
		Durable     bool   `json:"durable"`
		LastError   string `json:"last_error"`
		LastErrorAt string `json:"last_error_at,omitempty"`
		LastOKAt    string `json:"last_ok_at,omitempty"`
	}{
		Mode: v.Mode, Target: v.Target, Durable: v.Durable, LastError: v.LastErr,
		LastErrorAt: formatTimeOrEmpty(v.LastErrAt),
		LastOKAt:    formatTimeOrEmpty(v.LastOKAt),
	})
}

func formatTimeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
