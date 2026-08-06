package config

import (
	"bytes"
	"log/slog"
	"strings"
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

func TestAlpacaBaseURLWarning(t *testing.T) {
	cases := []struct {
		url  string
		warn bool
	}{
		{"https://data.alpaca.markets", false},        // correct
		{"https://data.alpaca.markets/", false},       // trailing slash is fine
		{"http://127.0.0.1:58399", false},             // local test stub, no path
		{"https://paper-api.alpaca.markets", true},    // wrong host (trading API)
		{"https://api.alpaca.markets", true},          // wrong host (trading API)
		{"https://paper-api.alpaca.markets/v2", true}, // wrong host + path
		{"https://data.alpaca.markets/v2", true},      // right host, stray path
	}
	for _, tc := range cases {
		c := &Config{AlpacaBaseURL: tc.url}
		got := c.AlpacaBaseURLWarning()
		if (got != "") != tc.warn {
			t.Errorf("AlpacaBaseURLWarning(%q) = %q; want warn=%v", tc.url, got, tc.warn)
		}
	}
}

func TestLoad_EmitsBaseURLWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	base := map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s"}

	bad := envFrom(map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s", "ALPACA_BASE_URL": "https://paper-api.alpaca.markets/v2"})
	if _, err := Load(bad); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "misconfigured") {
		t.Errorf("expected Load to warn on bad base URL; logs: %q", buf.String())
	}

	buf.Reset()
	if _, err := Load(envFrom(base)); err != nil { // default base URL is data.alpaca.markets
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "misconfigured") {
		t.Errorf("did not expect a warning for the default base URL; logs: %q", buf.String())
	}
}

func TestLoad_StorageDefaults(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageBackend != "memory" || cfg.StoragePath != "./data/cache.db" {
		t.Errorf("storage defaults wrong: %q %q", cfg.StorageBackend, cfg.StoragePath)
	}
}

func TestLoad_StorageValidation(t *testing.T) {
	_, err := Load(envFrom(map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s", "STORAGE": "s3"}))
	if err == nil {
		t.Fatal("expected error for invalid STORAGE")
	}
	cfg, err := Load(envFrom(map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s", "STORAGE": "disk", "STORAGE_PATH": "/tmp/x.db"}))
	if err != nil || cfg.StorageBackend != "disk" || cfg.StoragePath != "/tmp/x.db" {
		t.Fatalf("disk config wrong: %+v err=%v", cfg, err)
	}
}
