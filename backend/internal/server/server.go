// Package server wires the HTTP API: display fetch, telemetry ingest, metrics, health.
package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/kube"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

// sessionTTL is how long an admin/manager login lasts before requiring re-authentication.
const sessionTTL = 12 * time.Hour

type Server struct {
	cfg      *config.Live
	cache    *cache.Store
	tlm      *telemetry.Store
	alerts   *notify.Manager
	sessions *auth.SessionStore
	kube     *kube.Client // nil when not running in-cluster (e.g. local/demo) — writes stay in-memory only
	log      *slog.Logger

	// derived caches, recomputed from cfg by refreshDerived on New and on every admin/manager
	// write (see admin.go/manager.go) — never read cfg's Rooms directly for these, or a write
	// that lands between requests would leave a stale token/name/credential behind.
	derivedMu sync.RWMutex
	names     map[string]string // device_id -> friendly room name (for alert messages)
	auth      *deviceAuth
	directory *auth.Directory
}

func New(cfg *config.Live, c *cache.Store, tlm *telemetry.Store, alerts *notify.Manager, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, cache: c, tlm: tlm, alerts: alerts, sessions: auth.NewSessionStore(sessionTTL), log: log}
	s.refreshDerived(cfg.Load())
	if kc, err := kube.NewInClusterClient(); err == nil {
		s.kube = kc
	} else {
		log.Info("kube client unavailable — config writes will not persist to the ConfigMap", "err", err)
	}
	return s
}

// refreshDerived recomputes the device-token, device-name, and login-directory caches from cfg.
// Call this after every config.Live.Store so a room add/edit/delete, token rotation, or
// access-panel change (grant/revoke/re-role a user) takes effect immediately, no restart needed.
func (s *Server) refreshDerived(cfg *config.Config) {
	names := make(map[string]string, len(cfg.Rooms))
	for _, r := range cfg.Rooms {
		names[r.DeviceID] = r.Name
	}
	deviceAuthTable := newDeviceAuth(cfg)

	entries := make([]auth.Entry, len(cfg.Users))
	for i, u := range cfg.Users {
		entries[i] = auth.Entry{Username: u.Username, PasswordSHA256: u.PasswordSHA256, Role: auth.Role(u.Role), MustChangePassword: u.MustChangePassword, TOTPEnabled: u.TOTPSecret != ""}
	}
	directory := auth.NewDirectory(entries)

	s.derivedMu.Lock()
	s.names, s.auth, s.directory = names, deviceAuthTable, directory
	s.derivedMu.Unlock()
}

func (s *Server) deviceAuthTable() *deviceAuth {
	s.derivedMu.RLock()
	defer s.derivedMu.RUnlock()
	return s.auth
}

func (s *Server) roomName(deviceID string) string {
	s.derivedMu.RLock()
	defer s.derivedMu.RUnlock()
	return s.names[deviceID]
}

func (s *Server) userDirectory() *auth.Directory {
	s.derivedMu.RLock()
	defer s.derivedMu.RUnlock()
	return s.directory
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/display/{device}", s.handleDisplay)
	mux.HandleFunc("POST /api/v1/telemetry/{device}", s.handleTelemetry)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	// Cluster-internal-only, unauthenticated (same trust boundary as /metrics) — must never be
	// added to the public Ingress; see docs/DASHBOARD.md Decisions.
	mux.HandleFunc("GET /api/v1/status", s.handleStatusJSON)
	mux.HandleFunc("GET /status", s.handleStatusPage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /admin/login", s.handleLoginPage(adminUI))
	mux.HandleFunc("POST /admin/login", s.handleLoginSubmit(adminUI))
	mux.HandleFunc("POST /admin/logout", s.handleLogout(adminUI))
	mux.HandleFunc("GET /admin/change-password", s.requireRole(adminUI, s.handleChangePasswordPage(adminUI)))
	mux.HandleFunc("POST /admin/change-password", s.requireRole(adminUI, s.handleChangePasswordSubmit(adminUI)))
	mux.HandleFunc("GET /admin/verify-2fa", s.requireRole(adminUI, s.handleTOTPVerifyPage(adminUI)))
	mux.HandleFunc("POST /admin/verify-2fa", s.requireRole(adminUI, s.handleTOTPVerifySubmit(adminUI)))
	mux.HandleFunc("GET /admin/2fa/setup", s.requireRole(adminUI, s.handleTOTPSetupPage))
	mux.HandleFunc("POST /admin/2fa/enable", s.requireRole(adminUI, s.handleTOTPEnableSubmit))
	mux.HandleFunc("POST /admin/2fa/disable", s.requireRole(adminUI, s.handleTOTPDisableSubmit))
	mux.HandleFunc("GET /admin", s.requireRole(adminUI, s.handleAdminPage))
	mux.HandleFunc("POST /admin/rooms/save", s.requireRole(adminUI, s.handleAdminSaveRoom))
	mux.HandleFunc("POST /admin/rooms/delete", s.requireRole(adminUI, s.handleAdminDeleteRoom))
	mux.HandleFunc("POST /admin/wake/save", s.requireRole(adminUI, s.handleAdminSaveWakeDefaults))
	mux.HandleFunc("POST /admin/alerts/save", s.requireRole(adminUI, s.handleAdminSaveAlerts))
	mux.HandleFunc("POST /admin/firmware/save", s.requireRole(adminUI, s.handleAdminSaveFirmware))
	mux.HandleFunc("POST /admin/access/save", s.requireRole(adminUI, s.handleAdminSaveUser))
	mux.HandleFunc("POST /admin/access/delete", s.requireRole(adminUI, s.handleAdminDeleteUser))
	mux.HandleFunc("GET /manager/login", s.handleLoginPage(managerUI))
	mux.HandleFunc("POST /manager/login", s.handleLoginSubmit(managerUI))
	mux.HandleFunc("POST /manager/logout", s.handleLogout(managerUI))
	mux.HandleFunc("GET /manager/change-password", s.requireRole(managerUI, s.handleChangePasswordPage(managerUI)))
	mux.HandleFunc("POST /manager/change-password", s.requireRole(managerUI, s.handleChangePasswordSubmit(managerUI)))
	mux.HandleFunc("GET /manager/verify-2fa", s.requireRole(managerUI, s.handleTOTPVerifyPage(managerUI)))
	mux.HandleFunc("POST /manager/verify-2fa", s.requireRole(managerUI, s.handleTOTPVerifySubmit(managerUI)))
	mux.HandleFunc("GET /manager", s.requireRole(managerUI, s.handleManagerPage))
	mux.HandleFunc("POST /manager/wake/save", s.requireRole(managerUI, s.handleManagerSaveWake))
	mux.HandleFunc("GET /dashboard/login", s.handleLoginPage(viewerUI))
	mux.HandleFunc("POST /dashboard/login", s.handleLoginSubmit(viewerUI))
	mux.HandleFunc("POST /dashboard/logout", s.handleLogout(viewerUI))
	mux.HandleFunc("GET /dashboard/change-password", s.requireRole(viewerUI, s.handleChangePasswordPage(viewerUI)))
	mux.HandleFunc("POST /dashboard/change-password", s.requireRole(viewerUI, s.handleChangePasswordSubmit(viewerUI)))
	mux.HandleFunc("GET /dashboard/verify-2fa", s.requireRole(viewerUI, s.handleTOTPVerifyPage(viewerUI)))
	mux.HandleFunc("POST /dashboard/verify-2fa", s.requireRole(viewerUI, s.handleTOTPVerifySubmit(viewerUI)))
	mux.HandleFunc("GET /dashboard", s.requireRole(viewerUI, s.handleDashboardPage))
	mux.HandleFunc("GET /dashboard/preview/{device_id}", s.requireRole(viewerUI, s.handleDashboardPreview))
	// Optional: serve signed OTA images from a local dir at /firmware/<file>.bin. Integrity is
	// guaranteed by Secure Boot V2 signing, so this is unauthenticated (behind the internal ingress).
	if dir := s.cfg.Load().Firmware.Dir; dir != "" {
		mux.Handle("GET /firmware/", http.StripPrefix("/firmware/", http.FileServer(http.Dir(dir))))
		s.log.Info("serving OTA images", "dir", dir, "version", s.cfg.Load().Firmware.Version)
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
	cfg := s.cfg.Load()
	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      s.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 20 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	s.log.Info("broker listening", "addr", cfg.Listen, "rooms", len(cfg.Rooms))
	return srv.ListenAndServe()
}
