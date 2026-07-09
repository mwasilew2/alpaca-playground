package config

import (
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_RequiresCredentials(t *testing.T) {
	_, err := Load(envFrom(map[string]string{"ALPACA_API_KEY": "k"})) // secret missing
	if err == nil {
		t.Fatal("expected error when secret missing, got nil")
	}
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"ALPACA_API_KEY":    "k",
		"ALPACA_API_SECRET": "s",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AlpacaFeed != "iex" {
		t.Errorf("feed = %q, want iex", cfg.AlpacaFeed)
	}
	if cfg.PollInterval != 20*time.Second {
		t.Errorf("poll = %v, want 20s", cfg.PollInterval)
	}
	if cfg.Port != "8080" || cfg.PprofAddr != "127.0.0.1:6060" || cfg.CORSOrigin != "*" {
		t.Errorf("bad defaults: %+v", cfg)
	}
	if cfg.ServiceName != "alpaca-playground" {
		t.Errorf("service = %q", cfg.ServiceName)
	}
}

func TestLoad_ParsesWatchlist(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"ALPACA_API_KEY":    "k",
		"ALPACA_API_SECRET": "s",
		"WATCHLIST":         "AAPL, TSLA ,NVDA,",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"AAPL", "TSLA", "NVDA"}
	if len(cfg.Watchlist) != len(want) {
		t.Fatalf("watchlist = %v, want %v", cfg.Watchlist, want)
	}
	for i := range want {
		if cfg.Watchlist[i] != want[i] {
			t.Errorf("watchlist[%d] = %q, want %q", i, cfg.Watchlist[i], want[i])
		}
	}
}

func TestLoad_PprofDisableSentinel(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"ALPACA_API_KEY":    "k",
		"ALPACA_API_SECRET": "s",
		"PPROF_ADDR":        "-",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PprofAddr != "" {
		t.Errorf("PprofAddr = %q, want empty (disabled) for sentinel '-'", cfg.PprofAddr)
	}
}

func TestLoad_InvalidPollInterval(t *testing.T) {
	_, err := Load(envFrom(map[string]string{
		"ALPACA_API_KEY":    "k",
		"ALPACA_API_SECRET": "s",
		"POLL_INTERVAL":     "nope",
	}))
	if err == nil {
		t.Fatal("expected error for invalid POLL_INTERVAL")
	}
}
