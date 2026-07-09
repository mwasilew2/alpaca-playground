package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the validated runtime configuration.
type Config struct {
	AlpacaBaseURL string
	AlpacaKey     string
	AlpacaSecret  string
	AlpacaFeed    string
	Watchlist     []string
	PollInterval  time.Duration
	Port          string
	PprofAddr     string
	CORSOrigin    string
	OTLPEndpoint  string
	ServiceName   string
}

// Load reads configuration using the provided getenv function (os.Getenv in
// production, a fake in tests) and validates required fields.
func Load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		AlpacaBaseURL: def(getenv("ALPACA_BASE_URL"), "https://data.alpaca.markets"),
		AlpacaKey:     getenv("ALPACA_API_KEY"),
		AlpacaSecret:  getenv("ALPACA_API_SECRET"),
		AlpacaFeed:    def(getenv("ALPACA_FEED"), "iex"),
		Watchlist:     parseList(getenv("WATCHLIST")),
		Port:          def(getenv("PORT"), "8080"),
		PprofAddr:     defAllowEmpty(getenv, "PPROF_ADDR", ":6060"),
		CORSOrigin:    def(getenv("CORS_ORIGIN"), "*"),
		OTLPEndpoint:  getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		ServiceName:   def(getenv("OTEL_SERVICE_NAME"), "alpaca-playground"),
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
	return cfg, nil
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
