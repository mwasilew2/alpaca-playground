package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/mwasilew2/alpaca-playground/internal/alpaca"
	"github.com/mwasilew2/alpaca-playground/internal/config"
	"github.com/mwasilew2/alpaca-playground/internal/httpapi"
	"github.com/mwasilew2/alpaca-playground/internal/observability"
	"github.com/mwasilew2/alpaca-playground/internal/poller"
	"github.com/mwasilew2/alpaca-playground/internal/ranges"
	"github.com/mwasilew2/alpaca-playground/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Load .env into the process environment if present. Load (vs Overload) does
	// not override variables already set, so real env vars still win over .env.
	// Missing .env is not an error.
	_ = godotenv.Load()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownOtel, err := observability.Setup(ctx, cfg)
	if err != nil {
		return err
	}

	client, err := alpaca.New(cfg.AlpacaBaseURL, cfg.AlpacaKey, cfg.AlpacaSecret, cfg.AlpacaFeed,
		&http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return err
	}

	st := store.New(store.FetchFunc(client.GetBars), ranges.TTLForTimeframe)
	pl := poller.New(st, cfg.Watchlist, ranges.LiveTimeframes(), cfg.PollInterval)
	api := httpapi.New(st, cfg.Watchlist, cfg.CORSOrigin)

	apiSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	var wg sync.WaitGroup
	srvErr := make(chan error, 2)

	// Poller
	wg.Add(1)
	go func() { defer wg.Done(); pl.Run(ctx) }()

	// API server
	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("api server listening", "addr", apiSrv.Addr)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("api server: %w", err)
		}
	}()

	// Admin/pprof server (optional)
	var adminSrv *http.Server
	if cfg.PprofAddr != "" {
		adminSrv = observability.AdminServer(cfg.PprofAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.Info("admin server listening", "addr", adminSrv.Addr)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				srvErr <- fmt.Errorf("admin server: %w", err)
			}
		}()
	}

	// Block until a shutdown signal (ctx) OR a fatal server error.
	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-srvErr:
		slog.Error("server failed, shutting down", "err", err)
		runErr = err
	}
	// Cancel ctx so ctx-consumers (e.g. the poller) unblock even when we
	// arrived here via the srvErr branch rather than a signal.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(shutdownCtx)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutdownCtx)
	}
	wg.Wait()

	// Flush telemetry with its own budget so a slow HTTP drain can't starve it.
	otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otelCancel()
	if err := shutdownOtel(otelCtx); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}
