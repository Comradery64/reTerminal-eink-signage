// Command broker is the thin-client backend for the meeting-display fleet.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/configstore"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/poller"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/server"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config YAML")
	demo := flag.Bool("demo", false, "use a built-in fake schedule (no calendar provider) for local testing")

	// Every new flag defaults to "" (or false) and only overrides the YAML value when set, so a
	// flagless invocation of an existing deployment is byte-identical to before these existed.
	telemetryBackend := flag.String("telemetry-backend", "", `override telemetry.backend ("memory"|"sqlite"|"prometheus")`)
	telemetryDB := flag.String("telemetry-db", "", "override telemetry.sqlite.path")
	configPersistence := flag.String("config-persistence", "", `override config_persistence.mode ("auto"|"file"|"configmap"|"none")`)
	configWritePath := flag.String("config-write-path", "", "override config_persistence.file.path")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(1)
	}
	if *telemetryBackend != "" {
		cfg.Telemetry.Backend = *telemetryBackend
	}
	if *telemetryDB != "" {
		cfg.Telemetry.SQLite.Path = *telemetryDB
	}
	// configPersistenceModeSetExplicitly tracks whether the operator (flag or YAML) actually chose
	// a mode, as opposed to it merely defaulting to "auto" — -demo below must only force "none"
	// when nobody asked for anything else, or a deliberate `config_persistence: {mode: file}` next
	// to `-demo` would be silently overridden.
	configPersistenceModeSetExplicitly := cfg.ConfigPersistence.Mode != "" && cfg.ConfigPersistence.Mode != "auto"
	if *configPersistence != "" {
		cfg.ConfigPersistence.Mode = *configPersistence
		configPersistenceModeSetExplicitly = true
	}
	if *configWritePath != "" {
		cfg.ConfigPersistence.File.Path = *configWritePath
	}
	// -demo plus one click in /admin would otherwise rewrite a git-tracked example config on every
	// developer's first run through the README quick start — force "none" unless the operator
	// explicitly asked for durability alongside -demo.
	if *demo && !configPersistenceModeSetExplicitly {
		cfg.ConfigPersistence.Mode = "none"
	}
	live := config.NewLive(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var prov calendar.Provider
	switch {
	case *demo || cfg.Provider == "demo":
		log.Info("running in DEMO mode — built-in fake schedule, no calendar provider")
		prov = calendar.NewDemo()
	case cfg.Provider == "google":
		prov, err = calendar.NewGoogle(ctx, cfg.Google.CredentialsFile)
	}
	if err != nil {
		log.Error("calendar provider init failed", "provider", cfg.Provider, "err", err)
		os.Exit(1)
	}

	store := cache.New()
	tlm := newTelemetryStore(cfg.Telemetry, log)
	defer tlm.Close()
	tlm.Run(ctx, telemetryRetention(cfg.Telemetry), cfg.Telemetry.SQLite.PruneInterval)

	rend := render.New(cfg.Render.Width, cfg.Render.Height, cfg.Render.Dither)

	p := poller.New(live, prov, rend, store, tlm, log)
	go p.Run(ctx)

	alerts := notify.NewManager(cfg.Alerts.WebhookURL, cfg.Alerts.LowBatteryPct,
		cfg.Alerts.ClearPct, cfg.Alerts.MinRenotify, log)

	persist, err := configstore.New(configstore.Options{
		Mode:       cfg.ConfigPersistence.Mode,
		ConfigPath: *cfgPath,
		FilePath:   cfg.ConfigPersistence.File.Path,
		ConfigMap: configstore.ConfigMapOptions{
			Namespace: cfg.ConfigPersistence.ConfigMap.Namespace,
			Name:      cfg.ConfigPersistence.ConfigMap.Name,
			Key:       cfg.ConfigPersistence.ConfigMap.Key,
		},
		Log: log,
	})
	if err != nil {
		// An explicit mode that can't be honored (e.g. mode: configmap outside a cluster, or
		// mode: file with an unwritable path) is a misconfiguration, not a degraded-but-running
		// state — fail fast here the same way a config.Load failure already does, rather than
		// silently starting up in a mode nobody asked for.
		log.Error("config persistence init failed", "err", err)
		os.Exit(1)
	}

	srv := server.New(live, store, tlm, alerts, persist, log)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Error("http server stopped", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
}

// newTelemetryStore builds the *telemetry.Store for the configured backend. "memory" and
// "prometheus" are the same Store under the hood — see telemetry_design's honest description of
// "prometheus" as a documentation-only alias — the only difference is the startup log line telling
// an operator where their history actually lives. "sqlite" falls back to memory on any open
// failure rather than exiting: telemetry history is observability, and a full disk or a bad mount
// must not take a room-display fleet offline (the opposite policy from config persistence, and
// deliberately so — see configstore.New's doc comment for that contrast).
func newTelemetryStore(cfg config.TelemetryConfig, log *slog.Logger) *telemetry.Store {
	switch cfg.Backend {
	case "sqlite":
		sink, err := telemetry.OpenSQLite(telemetry.SQLiteOptions{Path: cfg.SQLite.Path})
		if err != nil {
			log.Error("telemetry sqlite backend init failed — falling back to in-memory telemetry (history will not survive a restart)", "path", cfg.SQLite.Path, "err", err)
			return telemetry.New()
		}
		tlm, err := telemetry.NewWithSink(sink, log, cfg.SQLite.QueueSize)
		if err != nil {
			log.Error("telemetry sqlite warm-start failed — falling back to in-memory telemetry (history will not survive a restart)", "path", cfg.SQLite.Path, "err", err)
			sink.Close()
			return telemetry.New()
		}
		log.Info("telemetry backend=sqlite", "path", cfg.SQLite.Path, "retention_days", cfg.SQLite.RetentionDays)
		return tlm

	case "prometheus":
		log.Info("telemetry backend=prometheus — the broker keeps only the latest report per device in memory; history lives in your Prometheus TSDB, scraped from /metrics")
		return telemetry.New()

	default: // "memory", or "" pre-defaults — config.Load's applyDefaults already fills "" to "memory"
		return telemetry.New()
	}
}

// telemetryRetention turns RetentionDays into a duration Store.Run and OpenSQLite can use
// directly. 0 means "keep forever" (documented as unsupported-but-allowed), represented as 0
// duration, which Store.Run treats as "never prune".
func telemetryRetention(cfg config.TelemetryConfig) (retention time.Duration) {
	if cfg.SQLite.RetentionDays <= 0 {
		return 0
	}
	return time.Duration(cfg.SQLite.RetentionDays) * 24 * time.Hour
}
