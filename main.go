package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

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

	apiSrv := &http.Server{Addr: ":" + cfg.Port, Handler: api.Handler()}

	var wg sync.WaitGroup

	// Poller
	wg.Add(1)
	go func() { defer wg.Done(); pl.Run(ctx) }()

	// API server
	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("api server listening", "addr", apiSrv.Addr)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("api server error", "err", err)
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
				slog.Error("admin server error", "err", err)
			}
		}()
	}

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(shutdownCtx)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutdownCtx)
	}
	wg.Wait()
	return shutdownOtel(shutdownCtx)
}
