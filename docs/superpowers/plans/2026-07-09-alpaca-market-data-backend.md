# Alpaca Market Data Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers-extended-cc:subagent-driven-development (recommended) or superpowers-extended-cc:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go webserver that fetches OHLCV bars from the Alpaca Market Data API, caches them in memory keyed by (symbol, timeframe), keeps a watchlist warm via a background poller, and serves bar series over selectable time ranges as JSON — with OpenTelemetry logs/metrics/traces and pprof.

**Architecture:** Small single-purpose `internal/` packages behind narrow interfaces. A leaf `marketdata` package holds the domain `Bar` type. `alpaca.Client` wraps the generated oapi client (the only place touching `gen/oapi`). `store.Store` is a thread-safe read-through cache. `poller` warms the watchlist. `httpapi` serves our own routes. Components emit telemetry through OTel **global** providers set up once by `observability.Setup`, so no component imports the observability package.

**Tech Stack:** Go 1.26, stdlib `net/http` (ServeMux method patterns), `log/slog`, oapi-codegen generated client, OpenTelemetry (traces/metrics/logs, OTLP + stdout exporters), `otelhttp`, `otelslog`, `net/http/pprof`.

**User decisions (already made):**
- Purpose: live dashboard **backend** only (frontend is a later, separate spec).
- Data: historical **bars** (OHLCV) for charting over ranges `10m,1h,1d,1w,1mo,1y,5y,all`.
- Symbols: **watchlist + on-demand**.
- Freshness: **near-real-time ~15–30s** (default poll 20s).
- Feed: **IEX** (free), configurable, default `iex`.
- Store: **in-memory, keyed by (symbol, timeframe)** (approach A).
- Observability: **OpenTelemetry logs + metrics + traces, plus pprof**.
- pprof on a **separate admin port** (`:6060` default); OTLP exporter with **stdout fallback** when no endpoint configured.

---

## File Structure

```
alpaca-playground/
├── main.go                              # wire everything; start API + admin + poller; graceful shutdown
├── .env.example                         # documents env vars
├── internal/
│   ├── marketdata/bar.go                # leaf domain type: Bar
│   ├── config/config.go                 # Config struct + Load(getenv)
│   ├── config/config_test.go
│   ├── ranges/ranges.go                 # Range, Spec, Resolve, Slice, TTLForTimeframe, LiveTimeframes
│   ├── ranges/ranges_test.go
│   ├── alpaca/client.go                 # wraps gen/oapi client: auth, feed, GetBars (pagination + 429 retry)
│   ├── alpaca/client_test.go
│   ├── store/store.go                   # in-memory read-through cache keyed by (symbol, timeframe)
│   ├── store/store_test.go
│   ├── poller/poller.go                 # background watchlist refresher
│   ├── poller/poller_test.go
│   ├── httpapi/handlers.go              # JSON routes + CORS + otelhttp
│   ├── httpapi/handlers_test.go
│   └── observability/otel.go            # OTel providers/exporters + pprof admin server
└── gen/oapi/...                         # generated (existing)
```

**Cross-task type contract (define once, reuse verbatim):**

```go
// internal/marketdata/bar.go
type Bar struct {
    T  time.Time `json:"t"`
    O  float64   `json:"o"`
    H  float64   `json:"h"`
    L  float64   `json:"l"`
    C  float64   `json:"c"`
    V  int64     `json:"v"`
    N  int64     `json:"n"`
    VW float64   `json:"vw"`
}
```

Key signatures used across tasks:
- `ranges.Resolve(r Range) (Spec, error)`; `Spec{ Timeframe string; Lookback time.Duration }`; `Spec.Start(now time.Time) time.Time`
- `ranges.Slice(bars []marketdata.Bar, start time.Time) []marketdata.Bar`
- `ranges.TTLForTimeframe(tf string) time.Duration`; `ranges.LiveTimeframes() []string`
- `alpaca.New(baseURL, key, secret, feed string, doer oapi.HttpRequestDoer) (*alpaca.Client, error)`
- `(*alpaca.Client).GetBars(ctx, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)`
- `store.FetchFunc = func(ctx, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)`
- `store.New(fetch FetchFunc, ttl func(string) time.Duration) *store.Store`
- `(*store.Store).Get(ctx, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)`

---

### Task 1: Domain type, config, and Makefile test target

**Goal:** Create the leaf `marketdata.Bar` type and a validated `config.Config` loaded from env, plus `.env.example` and a `make test` target.

**Files:**
- Create: `internal/marketdata/bar.go`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `.env.example`
- Modify: `Makefile` (add `test` target)
- Modify: `.gitignore` (add `.env`)

**Acceptance Criteria:**
- [ ] `marketdata.Bar` exists with fields T,O,H,L,C,V,N,VW and JSON tags t/o/h/l/c/v/n/vw.
- [ ] `config.Load` returns an error when `ALPACA_API_KEY` or `ALPACA_API_SECRET` is missing.
- [ ] Defaults applied: feed `iex`, poll `20s`, port `8080`, pprof `:6060`, CORS `*`, service name `alpaca-playground`.
- [ ] `WATCHLIST="AAPL, TSLA ,NVDA"` parses to `["AAPL","TSLA","NVDA"]` (trimmed, empties dropped).

**Verify:** `go test ./internal/config/... -v` → PASS

**Steps:**

- [ ] **Step 1: Create the domain Bar type**

```go
// internal/marketdata/bar.go
package marketdata

import "time"

// Bar is a single OHLCV aggregate for one symbol over one timeframe interval.
type Bar struct {
	T  time.Time `json:"t"`  // bar start timestamp
	O  float64   `json:"o"`  // open
	H  float64   `json:"h"`  // high
	L  float64   `json:"l"`  // low
	C  float64   `json:"c"`  // close
	V  int64     `json:"v"`  // volume
	N  int64     `json:"n"`  // trade count
	VW float64   `json:"vw"` // volume-weighted average price
}
```

- [ ] **Step 2: Write the failing config test**

```go
// internal/config/config_test.go
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
	if cfg.Port != "8080" || cfg.PprofAddr != ":6060" || cfg.CORSOrigin != "*" {
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/config/... -v`
Expected: FAIL (package `config` / `Load` undefined).

- [ ] **Step 4: Implement config**

```go
// internal/config/config.go
package config

import (
	"errors"
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
			return nil, errors.New("invalid POLL_INTERVAL: " + err.Error())
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

// defAllowEmpty returns the fallback only when the var is unset; an explicit
// empty string is preserved (used by PPROF_ADDR="" to disable the admin server).
func defAllowEmpty(getenv func(string) string, key, fallback string) string {
	if _, isSet := lookup(getenv, key); isSet {
		return getenv(key)
	}
	return fallback
}

// lookup reports whether key resolves to a non-empty value; the getenv contract
// cannot distinguish unset from empty, so empty is treated as "use fallback"
// EXCEPT we special-case the literal sentinel "-" to mean "explicitly empty".
func lookup(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	if v == "-" {
		return "", true
	}
	return v, v != ""
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
```

> Note: to disable pprof, set `PPROF_ADDR=-` (the sentinel for "explicitly empty"); main treats an empty `PprofAddr` as "admin server off".

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Create `.env.example`**

```bash
# .env.example — copy to .env and fill in. .env is gitignored.
ALPACA_API_KEY=
ALPACA_API_SECRET=
ALPACA_FEED=iex
ALPACA_BASE_URL=https://data.alpaca.markets
WATCHLIST=AAPL,TSLA,NVDA
POLL_INTERVAL=20s
PORT=8080
PPROF_ADDR=:6060
CORS_ORIGIN=*
# Leave OTEL endpoint unset to use the stdout exporter for local dev.
OTEL_EXPORTER_OTLP_ENDPOINT=
OTEL_SERVICE_NAME=alpaca-playground
```

- [ ] **Step 7: Add Makefile test target and gitignore .env**

Add to `Makefile` (keep existing `fetch-spec`/`gen-oapi`):

```makefile
.PHONY: test run
test:
	go test ./... -race

run:
	go run .
```

Append `.env` to `.gitignore`.

- [ ] **Step 8: Commit**

```bash
git add internal/marketdata internal/config .env.example Makefile .gitignore
git commit --no-gpg-sign -m "feat: add domain Bar type and config loading"
```

```json:metadata
{"files": ["internal/marketdata/bar.go", "internal/config/config.go", "internal/config/config_test.go", ".env.example", "Makefile", ".gitignore"], "verifyCommand": "go test ./internal/config/... -v", "acceptanceCriteria": ["Bar type with correct JSON tags", "Load errors without credentials", "defaults applied", "watchlist parsed and trimmed"], "modelTier": "mechanical"}
```

---

### Task 2: Ranges package (range → timeframe mapping + slicing)

**Goal:** Pure, I/O-free logic mapping a range string to `{timeframe, lookback}`, computing a start time, slicing a bar series to a window, plus TTL-by-timeframe and the poller's live timeframes.

**Files:**
- Create: `internal/ranges/ranges.go`
- Create: `internal/ranges/ranges_test.go`

**Acceptance Criteria:**
- [ ] Every range in `{10m,1h,1d,1w,1mo,1y,5y,all}` resolves to the timeframe in the design table; unknown ranges return an error.
- [ ] `Spec.Start(now)` returns `now-Lookback`, and a fixed early epoch (2016-01-01 UTC) when `Lookback==0` (the `all` range).
- [ ] `Slice` returns only bars with `T >= start`, preserving order, on an ascending-sorted input.
- [ ] `TTLForTimeframe` returns shorter TTLs for intraday timeframes than for daily+; `LiveTimeframes` returns `{1Min,5Min,1Hour}`.

**Verify:** `go test ./internal/ranges/... -v` → PASS

**Steps:**

- [ ] **Step 1: Write the failing tests**

```go
// internal/ranges/ranges_test.go
package ranges

import (
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

func TestResolve_AllRanges(t *testing.T) {
	want := map[Range]string{
		R10m: "1Min", R1h: "1Min", R1d: "5Min", R1w: "1Hour",
		R1mo: "1Hour", R1y: "1Day", R5y: "1Week", RAll: "1Month",
	}
	for r, tf := range want {
		spec, err := Resolve(r)
		if err != nil {
			t.Fatalf("Resolve(%s) error: %v", r, err)
		}
		if spec.Timeframe != tf {
			t.Errorf("Resolve(%s).Timeframe = %q, want %q", r, spec.Timeframe, tf)
		}
	}
}

func TestResolve_Unknown(t *testing.T) {
	if _, err := Resolve("bogus"); err == nil {
		t.Fatal("expected error for unknown range")
	}
}

func TestSpecStart_AllUsesEpoch(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	spec, _ := Resolve(RAll)
	if got := spec.Start(now); !got.Equal(time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all Start = %v, want 2016-01-01", got)
	}
	spec1h, _ := Resolve(R1h)
	if got := spec1h.Start(now); !got.Equal(now.Add(-time.Hour)) {
		t.Errorf("1h Start = %v, want now-1h", got)
	}
}

func TestSlice(t *testing.T) {
	base := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	bars := []marketdata.Bar{
		{T: base}, {T: base.Add(time.Minute)}, {T: base.Add(2 * time.Minute)},
	}
	got := Slice(bars, base.Add(time.Minute))
	if len(got) != 2 || !got[0].T.Equal(base.Add(time.Minute)) {
		t.Errorf("Slice returned %d bars, first %v", len(got), got[0].T)
	}
	if len(Slice(bars, base.Add(time.Hour))) != 0 {
		t.Error("expected empty slice for start after all bars")
	}
}

func TestTTLAndLiveTimeframes(t *testing.T) {
	if TTLForTimeframe("1Min") >= TTLForTimeframe("1Day") {
		t.Error("intraday TTL should be shorter than daily TTL")
	}
	live := LiveTimeframes()
	if len(live) != 3 || live[0] != "1Min" || live[1] != "5Min" || live[2] != "1Hour" {
		t.Errorf("LiveTimeframes = %v", live)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/ranges/... -v`
Expected: FAIL (undefined `Resolve`, `Slice`, etc.).

- [ ] **Step 3: Implement ranges**

```go
// internal/ranges/ranges.go
package ranges

import (
	"fmt"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

// Range is a user-selectable chart time range.
type Range string

const (
	R10m Range = "10m"
	R1h  Range = "1h"
	R1d  Range = "1d"
	R1w  Range = "1w"
	R1mo Range = "1mo"
	R1y  Range = "1y"
	R5y  Range = "5y"
	RAll Range = "all"
)

// Spec is the resolved fetch plan for a range.
type Spec struct {
	Timeframe string        // Alpaca timeframe string, e.g. "1Min"
	Lookback  time.Duration // 0 means "all available history"
}

// epochStart bounds the "all" range; Alpaca stock history begins ~2016.
var epochStart = time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)

// Start returns the inclusive start time for this spec relative to now.
func (s Spec) Start(now time.Time) time.Time {
	if s.Lookback <= 0 {
		return epochStart
	}
	return now.Add(-s.Lookback)
}

const day = 24 * time.Hour

var table = map[Range]Spec{
	R10m: {Timeframe: "1Min", Lookback: 10 * time.Minute},
	R1h:  {Timeframe: "1Min", Lookback: time.Hour},
	R1d:  {Timeframe: "5Min", Lookback: day},
	R1w:  {Timeframe: "1Hour", Lookback: 7 * day},
	R1mo: {Timeframe: "1Hour", Lookback: 30 * day},
	R1y:  {Timeframe: "1Day", Lookback: 365 * day},
	R5y:  {Timeframe: "1Week", Lookback: 5 * 365 * day},
	RAll: {Timeframe: "1Month", Lookback: 0},
}

// Resolve maps a range to its fetch spec.
func Resolve(r Range) (Spec, error) {
	spec, ok := table[r]
	if !ok {
		return Spec{}, fmt.Errorf("unknown range %q", r)
	}
	return spec, nil
}

// Valid reports whether r is a known range.
func (r Range) Valid() bool {
	_, ok := table[r]
	return ok
}

// AllRanges returns every supported range (unspecified order).
func AllRanges() []Range {
	out := make([]Range, 0, len(table))
	for r := range table {
		out = append(out, r)
	}
	return out
}

// Slice returns the bars with T >= start. Input must be ascending by T.
func Slice(bars []marketdata.Bar, start time.Time) []marketdata.Bar {
	for i, b := range bars {
		if !b.T.Before(start) {
			return bars[i:]
		}
	}
	return nil
}

// TTLForTimeframe is the cache freshness window for a timeframe. Intraday data
// changes constantly; daily+ data changes at most once per day.
func TTLForTimeframe(tf string) time.Duration {
	switch tf {
	case "1Min":
		return 15 * time.Second
	case "5Min":
		return 30 * time.Second
	case "1Hour":
		return 5 * time.Minute
	case "1Day":
		return time.Hour
	case "1Week":
		return 12 * time.Hour
	case "1Month":
		return 24 * time.Hour
	default:
		return time.Minute
	}
}

// LiveTimeframes are the intraday timeframes the poller keeps warm.
func LiveTimeframes() []string {
	return []string{"1Min", "5Min", "1Hour"}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/ranges/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ranges
git commit --no-gpg-sign -m "feat: add range->timeframe mapping and bar slicing"
```

```json:metadata
{"files": ["internal/ranges/ranges.go", "internal/ranges/ranges_test.go"], "verifyCommand": "go test ./internal/ranges/... -v", "acceptanceCriteria": ["all ranges resolve to design-table timeframes", "unknown range errors", "all range uses 2016 epoch start", "Slice filters by T>=start", "TTL intraday < daily, LiveTimeframes = 1Min/5Min/1Hour"], "modelTier": "mechanical"}
```

---

### Task 3: Observability foundation (OpenTelemetry + pprof) and dependencies

**Goal:** Add the OpenTelemetry dependencies and implement `observability.Setup` — global Tracer/Meter/Logger providers over a shared resource, OTLP exporters with a stdout fallback, `slog` wired to OTel logs, and a helper to run the pprof admin server. This task adds the OTel modules the later tasks compile against.

**Files:**
- Create: `internal/observability/otel.go`
- Create: `internal/observability/otel_test.go`
- Modify: `go.mod` / `go.sum` (via `go get`)

**Acceptance Criteria:**
- [ ] `go get` adds the OTel SDK, OTLP + stdout exporters, `otelhttp`, and `otelslog`; `go build ./...` succeeds.
- [ ] `Setup(ctx, cfg)` returns a `shutdown func(context.Context) error` and sets the global tracer/meter/logger providers.
- [ ] With no `OTLPEndpoint`, Setup uses stdout exporters and does not error; `slog.Default()` routes through OTel.
- [ ] `AdminServer(addr)` returns an `*http.Server` whose mux serves `/debug/pprof/`.

**Verify:** `go test ./internal/observability/... -v` → PASS and `go build ./...` → no output

**Steps:**

- [ ] **Step 1: Add dependencies**

Run:

```bash
go get go.opentelemetry.io/otel@latest \
  go.opentelemetry.io/otel/sdk@latest \
  go.opentelemetry.io/otel/sdk/metric@latest \
  go.opentelemetry.io/otel/sdk/log@latest \
  go.opentelemetry.io/otel/log@latest \
  go.opentelemetry.io/otel/exporters/stdout/stdouttrace@latest \
  go.opentelemetry.io/otel/exporters/stdout/stdoutmetric@latest \
  go.opentelemetry.io/otel/exporters/stdout/stdoutlog@latest \
  go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@latest \
  go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp@latest \
  go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp@latest \
  go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@latest \
  go.opentelemetry.io/contrib/bridges/otelslog@latest
```

> If an exporter import path fails to resolve for the installed OTel version, run `go doc <module>` to find the current path and adjust; the structure below is stable across recent versions.

- [ ] **Step 2: Write the failing test**

```go
// internal/observability/otel_test.go
package observability

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/mwasilew2/alpaca-playground/internal/config"
)

func TestSetup_StdoutFallback(t *testing.T) {
	cfg := &config.Config{ServiceName: "test-svc"} // no OTLP endpoint
	shutdown, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Setup error: %v", err)
	}
	defer shutdown(context.Background())

	slog.Info("smoke", "k", "v") // must not panic
}

func TestAdminServer_ServesPprof(t *testing.T) {
	srv := AdminServer(":0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/debug/pprof/", nil)
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("pprof index status = %d, want 200", rec.Code)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/observability/... -v`
Expected: FAIL (undefined `Setup`, `AdminServer`).

- [ ] **Step 4: Implement observability**

```go
// internal/observability/otel.go
package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"

	"github.com/mwasilew2/alpaca-playground/internal/config"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	otellog "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup installs global Tracer/Meter/Logger providers and wires slog to OTel
// logs. With no OTLP endpoint configured it falls back to stdout exporters so
// telemetry is still visible during local development. The returned shutdown
// function flushes and closes all three providers.
func Setup(ctx context.Context, cfg *config.Config) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName(cfg.ServiceName),
	))
	if err != nil {
		return nil, err
	}

	useOTLP := cfg.OTLPEndpoint != ""

	// Traces
	var spanExp sdktrace.SpanExporter
	if useOTLP {
		spanExp, err = otlptracehttp.New(ctx)
	} else {
		spanExp, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
	}
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(spanExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)

	// Metrics
	var metricReader sdkmetric.Reader
	if useOTLP {
		me, e := otlpmetrichttp.New(ctx)
		if e != nil {
			return nil, e
		}
		metricReader = sdkmetric.NewPeriodicReader(me)
	} else {
		me, e := stdoutmetric.New()
		if e != nil {
			return nil, e
		}
		metricReader = sdkmetric.NewPeriodicReader(me)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader), sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)

	// Logs
	var logExp sdklog.Exporter
	if useOTLP {
		logExp, err = otlploghttp.New(ctx)
	} else {
		logExp, err = stdoutlog.New()
	}
	if err != nil {
		return nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	otellog.SetLoggerProvider(lp)
	slog.SetDefault(slog.New(otelslog.NewHandler(cfg.ServiceName)))

	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return shutdown, nil
}

// AdminServer builds the admin/pprof HTTP server. It is not started here.
func AdminServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return &http.Server{Addr: addr, Handler: mux}
}
```

> If `go build` reports a wrong import path (OTel occasionally renames the global logger package or semconv version), resolve with `go doc go.opentelemetry.io/otel/log/global` and pin `semconv` to the version present in `go.sum`. Do not change behavior — only import paths.

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/observability/... -v && go build ./...`
Expected: PASS, and build prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/observability go.mod go.sum
git commit --no-gpg-sign -m "feat: add OpenTelemetry setup and pprof admin server"
```

```json:metadata
{"files": ["internal/observability/otel.go", "internal/observability/otel_test.go", "go.mod", "go.sum"], "verifyCommand": "go test ./internal/observability/... -v && go build ./...", "acceptanceCriteria": ["otel deps added and build succeeds", "Setup returns shutdown and sets globals", "stdout fallback with no endpoint", "AdminServer serves /debug/pprof/"], "modelTier": "frontier"}
```

---

### Task 4: Alpaca client wrapper (auth, feed, bars with pagination + 429 retry)

**Goal:** Wrap the generated oapi client behind `alpaca.Client`, injecting auth headers and the feed param, exposing `GetBars` that pages through results, retries on 429, and converts `oapi.StockBar` → `marketdata.Bar`. This is the ONLY package that imports `gen/oapi`.

**Files:**
- Create: `internal/alpaca/client.go`
- Create: `internal/alpaca/client_test.go`

**Acceptance Criteria:**
- [ ] `New` builds a client that sets `APCA-API-KEY-ID` and `APCA-API-SECRET-KEY` headers and the `feed` query param on requests.
- [ ] `GetBars` follows `next_page_token` and returns the concatenated, converted bars.
- [ ] A `429` response triggers a bounded backoff retry; exhausting retries returns an error.
- [ ] Non-2xx (e.g. 500) returns an error including the status code; the outbound `http.Client` uses the `otelhttp` transport.

**Verify:** `go test ./internal/alpaca/... -v` → PASS

**Steps:**

- [ ] **Step 1: Write the failing tests (against httptest)**

```go
// internal/alpaca/client_test.go
package alpaca

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points the client at ts and uses a fast (near-zero) backoff.
func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	c, err := New(ts.URL, "key-123", "secret-456", "iex", ts.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.backoff = func(int) time.Duration { return time.Millisecond }
	c.maxRetries = 3
	return c
}

func TestGetBars_AuthFeedAndPagination(t *testing.T) {
	var gotKey, gotSecret, gotFeed string
	page := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("APCA-API-KEY-ID")
		gotSecret = r.Header.Get("APCA-API-SECRET-KEY")
		gotFeed = r.URL.Query().Get("feed")
		w.Header().Set("Content-Type", "application/json")
		if page == 0 {
			page++
			json.NewEncoder(w).Encode(map[string]any{
				"bars": map[string]any{"AAPL": []map[string]any{
					{"t": "2026-07-09T13:30:00Z", "o": 1, "h": 2, "l": 0.5, "c": 1.5, "v": 100, "n": 10, "vw": 1.2},
				}},
				"next_page_token": "TOKEN2",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"bars": map[string]any{"AAPL": []map[string]any{
				{"t": "2026-07-09T13:31:00Z", "o": 1.5, "h": 2.5, "l": 1, "c": 2, "v": 200, "n": 20, "vw": 1.8},
			}},
			"next_page_token": nil,
		})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	start := time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	bars, err := c.GetBars(context.Background(), "AAPL", "1Min", start, end)
	if err != nil {
		t.Fatalf("GetBars: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2 (pagination)", len(bars))
	}
	if bars[0].C != 1.5 || bars[1].V != 200 {
		t.Errorf("conversion wrong: %+v", bars)
	}
	if gotKey != "key-123" || gotSecret != "secret-456" || gotFeed != "iex" {
		t.Errorf("auth/feed not sent: key=%q secret=%q feed=%q", gotKey, gotSecret, gotFeed)
	}
}

func TestGetBars_RetriesOn429(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"bars": map[string]any{"AAPL": []map[string]any{{"t": "2026-07-09T13:30:00Z", "c": 1}}},
			"next_page_token": nil,
		})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	bars, err := c.GetBars(context.Background(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if len(bars) != 1 || calls.Load() != 3 {
		t.Errorf("calls=%d bars=%d, want 3 calls and 1 bar", calls.Load(), len(bars))
	}
}

func TestGetBars_ErrorsOn500(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	if _, err := c.GetBars(context.Background(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("expected error on 500")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/alpaca/... -v`
Expected: FAIL (undefined `New`, `Client`, `GetBars`).

- [ ] **Step 3: Implement the client**

```go
// internal/alpaca/client.go
package alpaca

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/mwasilew2/alpaca-playground/gen/oapi"
	"github.com/mwasilew2/alpaca-playground/internal/marketdata"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Client wraps the generated Alpaca market-data client with auth, feed, and the
// domain GetBars method. It is the only package that imports gen/oapi.
type Client struct {
	oc         *oapi.ClientWithResponses
	feed       oapi.StockHistoricalFeed
	maxRetries int
	backoff    func(attempt int) time.Duration
}

// New builds a Client. doer is the underlying HTTP doer (e.g. an *http.Client);
// its transport is wrapped with otelhttp so outbound calls are traced/metered.
func New(baseURL, key, secret, feed string, doer oapi.HttpRequestDoer) (*Client, error) {
	hc, ok := doer.(*http.Client)
	if !ok || hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	traced := *hc
	traced.Transport = otelhttp.NewTransport(hc.Transport)

	auth := func(ctx context.Context, req *http.Request) error {
		req.Header.Set("APCA-API-KEY-ID", key)
		req.Header.Set("APCA-API-SECRET-KEY", secret)
		return nil
	}
	oc, err := oapi.NewClientWithResponses(baseURL,
		oapi.WithHTTPClient(&traced),
		oapi.WithRequestEditorFn(auth),
	)
	if err != nil {
		return nil, err
	}
	return &Client{
		oc:         oc,
		feed:       oapi.StockHistoricalFeed(feed),
		maxRetries: 4,
		backoff:    func(attempt int) time.Duration { return time.Duration(attempt) * 500 * time.Millisecond },
	}, nil
}

// GetBars fetches all bars for one symbol/timeframe in [start,end], following
// pagination and retrying on HTTP 429.
func (c *Client) GetBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	var out []marketdata.Bar
	var pageToken *string

	for {
		params := &oapi.StockBarsParams{
			Symbols:   symbol,
			TimeFrame: timeframe,
			Start:     &start,
			End:       &end,
			Feed:      &c.feed,
			PageToken: pageToken,
		}

		resp, err := c.doWithRetry(ctx, params)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			return nil, fmt.Errorf("alpaca stock bars: unexpected status %d", resp.StatusCode())
		}

		for _, sb := range resp.JSON200.Bars[symbol] {
			out = append(out, convert(sb))
		}

		if resp.JSON200.NextPageToken == nil || *resp.JSON200.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.JSON200.NextPageToken
	}
}

func (c *Client) doWithRetry(ctx context.Context, params *oapi.StockBarsParams) (*oapi.StockBarsResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.backoff(attempt)):
			}
		}
		resp, err := c.oc.StockBarsWithResponse(ctx, params)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode() == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("alpaca rate limited (429)")
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("alpaca request failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

func convert(sb oapi.StockBar) marketdata.Bar {
	return marketdata.Bar{
		T:  sb.Timestamp,
		O:  sb.Open,
		H:  sb.High,
		L:  sb.Low,
		C:  sb.Close,
		V:  sb.Volume,
		N:  sb.TradeCount,
		VW: sb.VWAP,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/alpaca/... -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/alpaca
git commit --no-gpg-sign -m "feat: add Alpaca client wrapper with pagination and 429 retry"
```

```json:metadata
{"files": ["internal/alpaca/client.go", "internal/alpaca/client_test.go"], "verifyCommand": "go test ./internal/alpaca/... -v", "acceptanceCriteria": ["auth headers + feed param sent", "pagination followed via next_page_token", "429 triggers bounded retry", "500 returns error", "otelhttp transport used"], "modelTier": "standard"}
```

---

### Task 5: In-memory store (read-through cache keyed by symbol+timeframe)

**Goal:** Implement a thread-safe read-through cache. On a fresh hit that covers the requested start it returns cached bars; otherwise it invokes the fetch func, stores, and returns. Emits cache hit/miss counters via the global OTel meter.

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/store_test.go`

**Acceptance Criteria:**
- [ ] Second `Get` within the timeframe TTL and covered window does NOT call the fetch func (cache hit).
- [ ] A `Get` after the entry is stale re-invokes the fetch func.
- [ ] A `Get` requesting an earlier start than the cached window re-fetches (coverage miss).
- [ ] Concurrent `Get` calls are race-free under `-race`.

**Verify:** `go test ./internal/store/... -race -v` → PASS

**Steps:**

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/store_test.go
package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

func fixedTTL(d time.Duration) func(string) time.Duration {
	return func(string) time.Duration { return d }
}

func TestGet_CacheHitSkipsFetch(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return []marketdata.Bar{{T: start}}, nil
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := New(fetch, fixedTTL(time.Minute))
	s.now = func() time.Time { return now }

	start := now.Add(-time.Hour)
	if _, err := s.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1 (second should hit cache)", calls.Load())
	}
}

func TestGet_StaleRefetches(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return nil, nil
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := New(fetch, fixedTTL(time.Minute))
	cur := now
	s.now = func() time.Time { return cur }

	start := now.Add(-time.Hour)
	s.Get(context.Background(), "AAPL", "1Min", start, now)
	cur = now.Add(2 * time.Minute) // advance past TTL
	s.Get(context.Background(), "AAPL", "1Min", start, cur)
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2 (stale refetch)", calls.Load())
	}
}

func TestGet_EarlierStartRefetches(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return nil, nil
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := New(fetch, fixedTTL(time.Hour))
	s.now = func() time.Time { return now }

	s.Get(context.Background(), "AAPL", "1Day", now.Add(-24*time.Hour), now)
	s.Get(context.Background(), "AAPL", "1Day", now.Add(-48*time.Hour), now) // wider window
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2 (coverage miss)", calls.Load())
	}
}

func TestGet_Concurrent(t *testing.T) {
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		return []marketdata.Bar{{T: start}}, nil
	}
	s := New(fetch, fixedTTL(time.Minute))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Get(context.Background(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now())
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/... -v`
Expected: FAIL (undefined `New`, `Store`, `Get`).

- [ ] **Step 3: Implement the store**

```go
// internal/store/store.go
package store

import (
	"context"
	"sync"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// FetchFunc loads bars for one symbol/timeframe over [start,end] from upstream.
type FetchFunc func(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)

type entry struct {
	bars      []marketdata.Bar
	start     time.Time // earliest start this entry covers
	fetchedAt time.Time
}

// Store is a thread-safe read-through cache keyed by (symbol, timeframe).
type Store struct {
	mu    sync.Mutex
	data  map[string]entry
	fetch FetchFunc
	ttl   func(timeframe string) time.Duration
	now   func() time.Time

	hits   metric.Int64Counter
	misses metric.Int64Counter
}

// New creates a Store. ttl maps a timeframe to its freshness window.
func New(fetch FetchFunc, ttl func(string) time.Duration) *Store {
	m := otel.Meter("alpaca-playground/store")
	hits, _ := m.Int64Counter("store.cache.hits")
	misses, _ := m.Int64Counter("store.cache.misses")
	return &Store{
		data:   make(map[string]entry),
		fetch:  fetch,
		ttl:    ttl,
		now:    time.Now,
		hits:   hits,
		misses: misses,
	}
}

func key(symbol, timeframe string) string { return symbol + "|" + timeframe }

// Get returns bars for the symbol/timeframe covering [start,end]. It serves a
// fresh, covering cache entry, otherwise fetches upstream and caches the result.
func (s *Store) Get(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	k := key(symbol, timeframe)
	now := s.now()

	s.mu.Lock()
	e, ok := s.data[k]
	fresh := ok && now.Sub(e.fetchedAt) < s.ttl(timeframe) && !e.start.After(start)
	if fresh {
		bars := e.bars
		s.mu.Unlock()
		if s.hits != nil {
			s.hits.Add(ctx, 1)
		}
		return bars, nil
	}
	s.mu.Unlock()

	if s.misses != nil {
		s.misses.Add(ctx, 1)
	}
	bars, err := s.fetch(ctx, symbol, timeframe, start, end)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.data[k] = entry{bars: bars, start: start, fetchedAt: now}
	s.mu.Unlock()
	return bars, nil
}
```

> Note on stampede: two concurrent misses for the same key may both fetch. Acceptable for this scale; a `singleflight` guard is a future optimization, not needed now.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/... -race -v`
Expected: PASS (4 tests), no race warnings.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit --no-gpg-sign -m "feat: add in-memory read-through bar cache"
```

```json:metadata
{"files": ["internal/store/store.go", "internal/store/store_test.go"], "verifyCommand": "go test ./internal/store/... -race -v", "acceptanceCriteria": ["fresh covered hit skips fetch", "stale entry refetches", "earlier start refetches", "race-free concurrent Get"], "modelTier": "standard"}
```

---

### Task 6: Watchlist poller

**Goal:** A background goroutine that, on a ticker, refreshes each watchlist symbol's intraday timeframes through the store so served data stays near-real-time. Per-symbol errors are logged and never stop the loop. Emits tick/error metrics.

**Files:**
- Create: `internal/poller/poller.go`
- Create: `internal/poller/poller_test.go`

**Acceptance Criteria:**
- [ ] `Run(ctx)` refreshes every symbol × live timeframe once per tick and returns when `ctx` is cancelled.
- [ ] An immediate first refresh happens on `Run` start (not only after the first interval).
- [ ] A fetch error for one symbol does not stop refreshing the others or the loop.

**Verify:** `go test ./internal/poller/... -race -v` → PASS

**Steps:**

- [ ] **Step 1: Write the failing tests**

```go
// internal/poller/poller_test.go
package poller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/store"
)

func TestPoller_RefreshesAllSymbolsAndTimeframes(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		mu.Lock()
		seen[sym+"|"+tf]++
		mu.Unlock()
		return nil, nil
	}
	st := store.New(fetch, func(string) time.Duration { return 0 }) // TTL 0 => always refetch
	p := New(st, []string{"AAPL", "TSLA"}, []string{"1Min", "5Min"}, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	// Wait for the immediate first refresh to populate all 4 combos.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d combos refreshed, want 4", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestPoller_ErrorDoesNotStopLoop(t *testing.T) {
	var mu sync.Mutex
	count := 0
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		mu.Lock()
		count++
		mu.Unlock()
		if sym == "BAD" {
			return nil, errors.New("boom")
		}
		return nil, nil
	}
	st := store.New(fetch, func(string) time.Duration { return 0 })
	p := New(st, []string{"BAD", "GOOD"}, []string{"1Min"}, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if count < 2 {
		t.Errorf("expected both symbols attempted despite error, got %d", count)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/poller/... -v`
Expected: FAIL (undefined `New`, `Run`).

- [ ] **Step 3: Implement the poller**

```go
// internal/poller/poller.go
package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/store"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// windowFor is how much history the poller keeps warm per intraday timeframe.
func windowFor(tf string) time.Duration {
	switch tf {
	case "1Min":
		return time.Hour
	case "5Min":
		return 24 * time.Hour
	case "1Hour":
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// Poller periodically refreshes watchlist data through the store.
type Poller struct {
	store      *store.Store
	symbols    []string
	timeframes []string
	interval   time.Duration
	now        func() time.Time

	ticks  metric.Int64Counter
	errs   metric.Int64Counter
}

// New builds a Poller for the given symbols and (live) timeframes.
func New(s *store.Store, symbols, timeframes []string, interval time.Duration) *Poller {
	m := otel.Meter("alpaca-playground/poller")
	ticks, _ := m.Int64Counter("poller.ticks")
	errs, _ := m.Int64Counter("poller.errors")
	return &Poller{
		store:      s,
		symbols:    symbols,
		timeframes: timeframes,
		interval:   interval,
		now:        time.Now,
		ticks:      ticks,
		errs:       errs,
	}
}

// Run refreshes immediately, then every interval, until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.refresh(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refresh(ctx)
		}
	}
}

func (p *Poller) refresh(ctx context.Context) {
	if p.ticks != nil {
		p.ticks.Add(ctx, 1)
	}
	now := p.now()
	for _, sym := range p.symbols {
		for _, tf := range p.timeframes {
			start := now.Add(-windowFor(tf))
			if _, err := p.store.Get(ctx, sym, tf, start, now); err != nil {
				if p.errs != nil {
					p.errs.Add(ctx, 1)
				}
				slog.Warn("poller refresh failed", "symbol", sym, "timeframe", tf, "err", err)
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/poller/... -race -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/poller
git commit --no-gpg-sign -m "feat: add watchlist poller"
```

```json:metadata
{"files": ["internal/poller/poller.go", "internal/poller/poller_test.go"], "verifyCommand": "go test ./internal/poller/... -race -v", "acceptanceCriteria": ["refreshes all symbol x timeframe per tick", "immediate first refresh", "per-symbol error does not stop loop", "returns on ctx cancel"], "modelTier": "standard"}
```

---

### Task 7: HTTP API (handlers, CORS, otelhttp)

**Goal:** Serve `/healthz`, `/bars`, and `/watchlist` as JSON via a stdlib ServeMux, with a permissive CORS middleware and an `otelhttp`-wrapped handler. `/bars` validates input, resolves the range, fetches via the store, and slices to the requested window.

**Files:**
- Create: `internal/httpapi/handlers.go`
- Create: `internal/httpapi/handlers_test.go`

**Acceptance Criteria:**
- [ ] `GET /healthz` → 200 `{"status":"ok"}`.
- [ ] `GET /bars?symbol=AAPL&range=1d` → 200 with `{symbol,range,timeframe,bars}`; bars sliced to the range's start.
- [ ] Missing/invalid `symbol` or `range` → 400 with a JSON error.
- [ ] An upstream (store) error → 502; the CORS header is present on responses.
- [ ] `GET /watchlist` → 200 with the configured symbols array.

**Verify:** `go test ./internal/httpapi/... -v` → PASS

**Steps:**

- [ ] **Step 1: Write the failing tests**

```go
// internal/httpapi/handlers_test.go
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

// fakeStore implements the barSource interface used by the server.
type fakeStore struct {
	bars []marketdata.Bar
	err  error
}

func (f fakeStore) Get(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	return f.bars, f.err
}

func newTestServer(src barSource) http.Handler {
	s := New(src, []string{"AAPL", "TSLA"}, "*")
	s.now = func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }
	return s.Handler()
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(fakeStore{}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() == "" {
		t.Fatalf("healthz: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestBars_OK(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	src := fakeStore{bars: []marketdata.Bar{
		{T: now.Add(-48 * time.Hour)}, // older than 1d window -> sliced out
		{T: now.Add(-1 * time.Hour)},  // within 1d window
	}}
	rec := httptest.NewRecorder()
	newTestServer(src).ServeHTTP(rec, httptest.NewRequest("GET", "/bars?symbol=AAPL&range=1d", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Symbol    string            `json:"symbol"`
		Range     string            `json:"range"`
		Timeframe string            `json:"timeframe"`
		Bars      []marketdata.Bar  `json:"bars"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Symbol != "AAPL" || resp.Range != "1d" || resp.Timeframe != "5Min" {
		t.Errorf("bad envelope: %+v", resp)
	}
	if len(resp.Bars) != 1 {
		t.Errorf("bars=%d, want 1 after slicing", len(resp.Bars))
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
}

func TestBars_Validation(t *testing.T) {
	for _, q := range []string{"/bars", "/bars?symbol=AAPL", "/bars?symbol=AAPL&range=bogus", "/bars?range=1d"} {
		rec := httptest.NewRecorder()
		newTestServer(fakeStore{}).ServeHTTP(rec, httptest.NewRequest("GET", q, nil))
		if rec.Code != 400 {
			t.Errorf("%s: code=%d, want 400", q, rec.Code)
		}
	}
}

func TestBars_UpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	src := fakeStore{err: errors.New("boom")}
	newTestServer(src).ServeHTTP(rec, httptest.NewRequest("GET", "/bars?symbol=AAPL&range=1d", nil))
	if rec.Code != 502 {
		t.Errorf("code=%d, want 502", rec.Code)
	}
}

func TestWatchlist(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(fakeStore{}).ServeHTTP(rec, httptest.NewRequest("GET", "/watchlist", nil))
	var syms []string
	json.Unmarshal(rec.Body.Bytes(), &syms)
	if len(syms) != 2 || syms[0] != "AAPL" {
		t.Errorf("watchlist = %v", syms)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/httpapi/... -v`
Expected: FAIL (undefined `New`, `barSource`, `Handler`).

- [ ] **Step 3: Implement the handlers**

```go
// internal/httpapi/handlers.go
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/ranges"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// barSource is the store dependency the API needs (satisfied by *store.Store).
type barSource interface {
	Get(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)
}

// Server holds the API dependencies.
type Server struct {
	src        barSource
	watchlist  []string
	corsOrigin string
	now        func() time.Time
}

// New builds the API server.
func New(src barSource, watchlist []string, corsOrigin string) *Server {
	return &Server{src: src, watchlist: watchlist, corsOrigin: corsOrigin, now: time.Now}
}

// Handler returns the fully-wrapped HTTP handler (routes + CORS + otelhttp).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /bars", s.handleBars)
	mux.HandleFunc("GET /watchlist", s.handleWatchlist)
	return otelhttp.NewHandler(s.cors(mux), "http.server")
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleWatchlist(w http.ResponseWriter, r *http.Request) {
	syms := s.watchlist
	if syms == nil {
		syms = []string{}
	}
	writeJSON(w, http.StatusOK, syms)
}

type barsResponse struct {
	Symbol    string           `json:"symbol"`
	Range     string           `json:"range"`
	Timeframe string           `json:"timeframe"`
	Bars      []marketdata.Bar `json:"bars"`
}

func (s *Server) handleBars(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	rangeStr := r.URL.Query().Get("range")
	if symbol == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: symbol")
		return
	}
	rng := ranges.Range(rangeStr)
	spec, err := ranges.Resolve(rng)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid range: "+rangeStr)
		return
	}

	now := s.now()
	start := spec.Start(now)
	bars, err := s.src.Get(r.Context(), symbol, spec.Timeframe, start, now)
	if err != nil {
		slog.Error("bars fetch failed", "symbol", symbol, "range", rangeStr, "err", err)
		writeError(w, http.StatusBadGateway, "upstream market data error")
		return
	}

	out := ranges.Slice(bars, start)
	if out == nil {
		out = []marketdata.Bar{}
	}
	writeJSON(w, http.StatusOK, barsResponse{
		Symbol:    symbol,
		Range:     rangeStr,
		Timeframe: spec.Timeframe,
		Bars:      out,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/httpapi/... -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi
git commit --no-gpg-sign -m "feat: add HTTP API handlers with CORS and otelhttp"
```

```json:metadata
{"files": ["internal/httpapi/handlers.go", "internal/httpapi/handlers_test.go"], "verifyCommand": "go test ./internal/httpapi/... -v", "acceptanceCriteria": ["healthz 200", "bars 200 sliced to range with envelope", "validation 400s", "upstream error 502", "watchlist array", "CORS header present"], "modelTier": "standard"}
```

---

### Task 8: Wire everything in main.go (startup + graceful shutdown)

**Goal:** Compose all components in `main.go`: load config, set up OTel, build the Alpaca client → store → poller → API server, start the API server, admin/pprof server, and poller, and shut everything down cleanly on SIGINT/SIGTERM.

**Files:**
- Modify: `main.go` (replace the hello-world stub)

**Acceptance Criteria:**
- [ ] `go build ./...` succeeds and `go vet ./...` is clean.
- [ ] Missing credentials cause a fast, clear startup failure (non-zero exit, logged reason).
- [ ] `SIGINT` triggers graceful shutdown: poller stops, both HTTP servers shut down, OTel flushes.
- [ ] Full test suite passes: `make test`.

**Verify:** `go build ./... && go vet ./... && make test` → PASS

**Steps:**

- [ ] **Step 1: Implement main.go**

```go
// main.go
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
```

- [ ] **Step 2: Build, vet, and run the full suite**

Run: `go build ./... && go vet ./... && make test`
Expected: build/vet silent; all package tests PASS under `-race`.

- [ ] **Step 3: Smoke test the startup failure path**

Run: `env -u ALPACA_API_KEY -u ALPACA_API_SECRET go run . ; echo "exit=$?"`
Expected: logs an error mentioning `ALPACA_API_KEY and ALPACA_API_SECRET are required` and `exit=1`.

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit --no-gpg-sign -m "feat: wire server, poller, and telemetry in main with graceful shutdown"
```

```json:metadata
{"files": ["main.go"], "verifyCommand": "go build ./... && go vet ./... && make test", "acceptanceCriteria": ["build and vet clean", "missing credentials fail fast with exit 1", "SIGINT graceful shutdown", "make test passes"], "modelTier": "standard"}
```

---

### Task 9: Codegen cleanup (drop unused server stubs)

**Goal:** Stop generating the unused `std-http-server` stubs (generated from Alpaca's own spec, which we don't serve). Set `std-http-server: false` and regenerate.

**Files:**
- Modify: `gen/oapi/codegen.cfg.yml`
- Regenerate: `gen/oapi/server-oapi.gen.go`

**Acceptance Criteria:**
- [ ] `codegen.cfg.yml` has `std-http-server: false`.
- [ ] After regeneration, `go build ./...` and `make test` still pass (the client + models we use remain).

**Verify:** `make gen-oapi && go build ./... && make test` → PASS

**Steps:**

- [ ] **Step 1: Edit the codegen config**

In `gen/oapi/codegen.cfg.yml`, change:

```yaml
  std-http-server: true
```

to:

```yaml
  std-http-server: false
```

- [ ] **Step 2: Regenerate**

Run: `make gen-oapi`
Expected: `gen/oapi/server-oapi.gen.go` regenerated without the `ServerInterface`/`HandlerFromMux` server code.

> If offline and `make fetch-spec` fails, the existing `gen/oapi/market-data-api.json` (already present, ~140KB) is sufficient — run only the codegen step:
> `go -C tools run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config ../gen/oapi/codegen.cfg.yml -o ../gen/oapi/server-oapi.gen.go ../gen/oapi/market-data-api.json`

- [ ] **Step 3: Build and test**

Run: `go build ./... && make test`
Expected: PASS (only `alpaca` uses the generated code — the client + models are untouched).

- [ ] **Step 4: Commit**

```bash
git add gen/oapi/codegen.cfg.yml gen/oapi/server-oapi.gen.go
git commit --no-gpg-sign -m "chore: stop generating unused std-http-server stubs"
```

```json:metadata
{"files": ["gen/oapi/codegen.cfg.yml", "gen/oapi/server-oapi.gen.go"], "verifyCommand": "go build ./... && make test", "acceptanceCriteria": ["std-http-server false", "regenerated code builds", "make test passes"], "modelTier": "mechanical"}
```

---

## Self-Review

**Spec coverage:**
- §3 packages/components → Tasks 1–8 (marketdata/config/ranges/alpaca/store/poller/httpapi/observability + main). ✓
- §4 API (`/healthz`, `/bars`, `/watchlist`, CORS, admin/pprof) → Task 7 + Task 3 (AdminServer) + Task 8 (wiring). ✓
- §5 range→timeframe table → Task 2 `table`. ✓
- §6 config/env vars → Task 1. ✓
- §7 observability (logs/traces/metrics, pprof) → Task 3 (setup) + custom metrics in Tasks 5/6 + otelhttp in Tasks 4/7. ✓
- §8 error handling (429 retry, 502, 400, poller isolation, graceful shutdown) → Tasks 4, 7, 6, 8. ✓
- §9 testing → tests in every task; `make test` runs `-race`. ✓
- §10 codegen cleanup → Task 9. ✓
- §11 dependencies → Task 3 `go get`. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code. ✓

**Type consistency:** `marketdata.Bar` fields (T/O/H/L/C/V/N/VW) used identically in alpaca `convert`, store, ranges, httpapi. `store.FetchFunc` signature matches `alpaca.(*Client).GetBars` exactly (used as `store.FetchFunc(client.GetBars)` in main). `ranges.Spec{Timeframe,Lookback}` + `Start(now)` used consistently in httpapi. `barSource` interface in httpapi matches `*store.Store.Get`. ✓

**Note:** `main.go` uses `store.FetchFunc(client.GetBars)` — this conversion is valid because `GetBars`'s signature is identical to `FetchFunc`. Confirmed against Task 4 and Task 5 signatures.
