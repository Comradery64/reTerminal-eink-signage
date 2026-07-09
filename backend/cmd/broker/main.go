// Command broker is the thin-client backend for the meeting-display fleet.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/poller"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/server"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config YAML")
	demo := flag.Bool("demo", false, "use a built-in fake schedule (no calendar provider) for local testing")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(1)
	}

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
	tlm := telemetry.New()
	rend := render.New(cfg.Render.Width, cfg.Render.Height, cfg.Render.Dither)

	p := poller.New(cfg, prov, rend, store, log)
	go p.Run(ctx)

	alerts := notify.NewManager(cfg.Alerts.WebhookURL, cfg.Alerts.LowBatteryPct,
		cfg.Alerts.ClearPct, cfg.Alerts.MinRenotify, log)

	srv := server.New(cfg, store, tlm, alerts, log)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Error("http server stopped", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
}
