package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// Config is the validated runtime configuration.
type Config struct {
	AlpacaBaseURL  string
	AlpacaKey      string
	AlpacaSecret   string
	AlpacaFeed     string
	Watchlist      []string
	PollInterval   time.Duration
	Port           string
	PprofAddr      string
	CORSOrigin     string
	OTLPEndpoint   string
	ServiceName    string
	StorageBackend string // "memory" (default) or "disk"
	StoragePath    string // SQLite file path when StorageBackend == "disk"
}

// Load reads configuration using the provided getenv function (os.Getenv in
// production, a fake in tests) and validates required fields.
func Load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		AlpacaBaseURL:  def(getenv("ALPACA_BASE_URL"), "https://data.alpaca.markets"),
		AlpacaKey:      getenv("ALPACA_API_KEY"),
		AlpacaSecret:   getenv("ALPACA_API_SECRET"),
		AlpacaFeed:     def(getenv("ALPACA_FEED"), "iex"),
		Watchlist:      parseList(getenv("WATCHLIST")),
		Port:           def(getenv("PORT"), "8080"),
		PprofAddr:      defAllowEmpty(getenv, "PPROF_ADDR", "127.0.0.1:6060"),
		CORSOrigin:     def(getenv("CORS_ORIGIN"), "*"),
		OTLPEndpoint:   getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		ServiceName:    def(getenv("OTEL_SERVICE_NAME"), "alpaca-playground"),
		StorageBackend: def(getenv("STORAGE"), "memory"),
		StoragePath:    def(getenv("STORAGE_PATH"), "./data/cache.db"),
	}

	interval := 20 * time.Second
	if raw := getenv("POLL_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid POLL_INTERVAL: %w", err)
		}
		interval = d
	}
	cfg.PollInterval = interval

	if cfg.AlpacaKey == "" || cfg.AlpacaSecret == "" {
		return nil, errors.New("ALPACA_API_KEY and ALPACA_API_SECRET are required")
	}

	if w := cfg.AlpacaBaseURLWarning(); w != "" {
		slog.Warn("ALPACA_BASE_URL looks misconfigured for market data",
			"url", cfg.AlpacaBaseURL, "detail", w)
	}

	if cfg.StorageBackend != "memory" && cfg.StorageBackend != "disk" {
		return nil, fmt.Errorf("STORAGE must be 'memory' or 'disk', got %q", cfg.StorageBackend)
	}

	return cfg, nil
}

// AlpacaBaseURLWarning returns a human-readable warning if AlpacaBaseURL looks
// misconfigured for the market-data API, or "" if it looks fine. The generated
// client appends the full path (e.g. /v2/stocks/bars), so the base URL must be
// the host only, and market data is served from data.alpaca.markets — not the
// trading hosts (paper-api.alpaca.markets / api.alpaca.markets). Non-Alpaca
// hosts (e.g. a local test stub) are not flagged as long as they carry no path.
func (c *Config) AlpacaBaseURLWarning() string {
	u, err := url.Parse(c.AlpacaBaseURL)
	if err != nil {
		return "could not parse ALPACA_BASE_URL: " + err.Error()
	}
	var msgs []string
	if host := u.Hostname(); strings.HasSuffix(host, "alpaca.markets") && host != "data.alpaca.markets" {
		msgs = append(msgs, "market data is served from data.alpaca.markets, not "+host+
			" (paper-api/api are the trading API)")
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		msgs = append(msgs, "base URL should have no path — the client already appends /v2/stocks/bars (got /"+p+")")
	}
	return strings.Join(msgs, "; ")
}

func def(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// defAllowEmpty returns the fallback when the var is unset/empty EXCEPT the
// literal sentinel "-" which means "explicitly empty" (used by PPROF_ADDR="-"
// to disable the admin server).
func defAllowEmpty(getenv func(string) string, key, fallback string) string {
	v := getenv(key)
	if v == "-" {
		return ""
	}
	if v == "" {
		return fallback
	}
	return v
}

func parseList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}
