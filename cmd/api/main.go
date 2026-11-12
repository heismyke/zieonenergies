// Command api runs the Zieon white-label metering API.
//
//	api            serve HTTP (runs pending migrations first)
//	api migrate    apply migrations and exit
//	api migrate-down  roll back the latest migration (development only)
//	api seed-pilot    provision sandbox pilot tenants (non-production only)
//	api mqtt          run only the MQTT device gateway (no HTTP, no workers)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heismyke/zieonenergies/internal/app"
	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/config"
	"github.com/heismyke/zieonenergies/internal/db"
	"github.com/heismyke/zieonenergies/internal/httpapi"
	"github.com/heismyke/zieonenergies/internal/mqttgw"
	"github.com/heismyke/zieonenergies/internal/payments"
	"github.com/heismyke/zieonenergies/internal/secure"
	"github.com/heismyke/zieonenergies/internal/vending"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd == "migrate-down" {
		if cfg.Env == "production" {
			return errors.New("migrate-down is disabled in production; restore from backup instead")
		}
		v, err := store.MigrateDown(ctx)
		log.Info("rolled back", "version", v)
		return err
	}
	applied, err := store.Migrate(ctx)
	if err != nil {
		return err
	}
	log.Info("migrations", "applied", applied)
	if cmd == "migrate" {
		return nil
	}

	svc, err := newService(cfg, store, log)
	if err != nil {
		return err
	}
	switch cmd {
	case "seed-pilot":
		res, err := svc.SeedPilot(ctx)
		for _, l := range res.Lines {
			fmt.Println(l)
		}
		return err
	case "mqtt":
		if !cfg.MQTTEnabled() {
			return errors.New("mqtt: set MQTT_TLS_ADDR, MQTT_WS_ADDR or MQTT_TCP_ADDR")
		}
		gw, err := mqttgw.New(svc, cfg, log)
		if err != nil {
			return err
		}
		return gw.Run(ctx)
	case "serve":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	if cfg.MQTTEnabled() {
		gw, err := mqttgw.New(svc, cfg, log)
		if err != nil {
			return err
		}
		go func() {
			if err := gw.Run(ctx); err != nil {
				log.Error("mqtt gateway stopped", "err", err)
			}
		}()
	}

	if cfg.WorkersEnabled {
		svc.RunWorkers(ctx)
	}
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.New(svc, log, defaultTenant()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "env", cfg.Env)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func newService(cfg config.Config, store *db.Store, log *slog.Logger) (*app.Service, error) {
	keys, err := secure.NewKeyring(cfg.DataKeys, cfg.DataKeyID)
	if err != nil {
		return nil, err
	}
	svc := &app.Service{
		DB: store, Keys: keys, Cfg: cfg, Log: log, Now: time.Now,
		Signer:   auth.NewSigner(cfg.JWTSigningKey, cfg.JWTKeyID, cfg.AccessTTL),
		Vending:  map[string]vending.Provider{},
		Payments: map[string]payments.Provider{},
	}
	if cfg.SandboxAllowed() {
		svc.Vending["sandbox"] = vending.NewSandbox()
		svc.Payments["sandbox"] = payments.Sandbox{}
	}
	if cfg.VTpassAPIKey != "" {
		svc.Vending["vtpass"] = vending.NewVTpass(cfg.VTpassBaseURL, cfg.VTpassAPIKey, cfg.VTpassPublicKey, cfg.VTpassSecretKey, cfg.VTpassFallbackPhone)
	}
	if cfg.PaystackSecretKey != "" {
		svc.Payments["paystack"] = payments.NewPaystack(cfg.PaystackSecretKey)
	}
	// svc.Vending["live"] is intentionally unset: it requires an authorised
	// STS / DisCo vending partner integration (see docs/METER_VENDING_FLOW.md).
	return svc, nil
}

func defaultTenant() string {
	if v := os.Getenv("DEFAULT_TENANT"); v != "" {
		return v
	}
	return "zenerlink"
}
