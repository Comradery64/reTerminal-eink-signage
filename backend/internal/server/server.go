// Package server wires the HTTP API: display fetch, telemetry ingest, metrics, health.
package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

type Server struct {
	cfg    *config.Config
	cache  *cache.Store
	tlm    *telemetry.Store
	alerts *notify.Manager
	names  map[string]string // device_id -> friendly room name (for alert messages)
	auth   *deviceAuth
	log    *slog.Logger
}

func New(cfg *config.Config, c *cache.Store, tlm *telemetry.Store, alerts *notify.Manager, log *slog.Logger) *Server {
	names := make(map[string]string, len(cfg.Rooms))
	for _, r := range cfg.Rooms {
		names[r.DeviceID] = r.Name
	}
	return &Server{cfg: cfg, cache: c, tlm: tlm, alerts: alerts, names: names, auth: newDeviceAuth(cfg), log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/display/{device}", s.handleDisplay)
	mux.HandleFunc("POST /api/v1/telemetry/{device}", s.handleTelemetry)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Optional: serve signed OTA images from a local dir at /firmware/<file>.bin. Integrity is
	// guaranteed by Secure Boot V2 signing, so this is unauthenticated (behind the internal ingress).
	if dir := s.cfg.Firmware.Dir; dir != "" {
		mux.Handle("GET /firmware/", http.StripPrefix("/firmware/", http.FileServer(http.Dir(dir))))
		s.log.Info("serving OTA images", "dir", dir, "version", s.cfg.Firmware.Version)
	}
	return s.recover(mux)
}

// recover keeps a single panicking request from taking down the broker for the whole fleet.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func setNoWake(w http.ResponseWriter, secs uint32) {
	w.Header().Set("X-Next-Wake", strconv.FormatUint(uint64(secs), 10))
}

func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:         s.cfg.Listen,
		Handler:      s.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 20 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	s.log.Info("broker listening", "addr", s.cfg.Listen, "rooms", len(s.cfg.Rooms))
	return srv.ListenAndServe()
}
