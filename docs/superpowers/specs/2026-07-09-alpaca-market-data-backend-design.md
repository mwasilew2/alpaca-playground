# Alpaca Market Data Backend — Design

**Date:** 2026-07-09
**Status:** Approved (design phase)
**Scope:** Backend only. A rich frontend is planned but is a separate, later spec.

## 1. Purpose

A Go webserver that acts as the backend for a **live market-data dashboard**. It
fetches historical OHLCV bars from the Alpaca Market Data API, caches them in
memory, and serves them as JSON to a (future) charting frontend. The user picks a
symbol and a time range (10m → all-time) and gets the bar series for that range.

A configured **watchlist** is kept near-real-time by a background poller; any other
symbol can be requested **on demand** and is fetched read-through and cached.

## 2. Established Requirements

- **Data source:** Alpaca historical **bars** (OHLCV) endpoint, IEX feed (free tier),
  feed configurable via env (default `iex`).
- **Ranges:** `10m, 1h, 1d, 1w, 1mo, 1y, 5y, all`, each mapped to an appropriate bar
  timeframe (see §5).
- **Symbols:** watchlist (actively refreshed) + on-demand (any symbol).
- **Freshness:** watchlist refreshed every ~15–30s (default `POLL_INTERVAL=20s`).
- **Storage:** in-memory, keyed by `(symbol, timeframe)` — read-through cache.
- **Observability:** OpenTelemetry logs + metrics + traces, plus `net/http/pprof`.
- **Reuse:** calls Alpaca via the existing oapi-codegen client in `gen/oapi`. We write
  our own HTTP routes (not the generated server stubs).

Non-goals (YAGNI for now): frontend, persistence across restarts, auth on our own API,
a separate live-quote endpoint, resampling/aggregation of bars.

## 3. Architecture

Small, single-purpose packages that communicate through narrow interfaces and can be
tested independently.

```
alpaca-playground/
├── main.go                      # wire everything; start API + admin + poller; graceful shutdown
├── internal/
│   ├── config/config.go         # load + validate env into a Config struct
│   ├── alpaca/client.go         # thin wrapper over gen/oapi client: auth, feed, GetBars()
│   ├── store/store.go           # in-memory cache keyed by (symbol, timeframe), thread-safe
│   ├── ranges/ranges.go         # range -> {timeframe, lookback}; slice bars to a range
│   ├── poller/poller.go         # background goroutine refreshing the watchlist
│   ├── httpapi/handlers.go      # our JSON routes (stdlib net/http ServeMux)
│   └── observability/otel.go    # OTel providers + exporters; pprof admin server helper
```

### Component responsibilities

- **config** — reads and validates env vars into a `Config` struct. Fails fast if the
  Alpaca API key/secret are missing. Never logs secrets.
- **alpaca.Client** — the **only** place that touches the generated `gen/oapi` code.
  Injects auth headers + feed param and exposes a small domain method:
  `GetBars(ctx, symbol, timeframe, start, end) ([]Bar, error)`. Its underlying
  `http.Client` uses the `otelhttp` transport so outbound calls are traced/metered.
  Handles Alpaca 429 rate limits with bounded backoff retries. Follows Alpaca bar
  pagination (`next_page_token`) until the requested window is complete.
- **store.Store** — `map[key]series` behind a `sync.RWMutex`, `key = {symbol, timeframe}`.
  Each entry holds the bar slice plus `fetchedAt`, and caches the widest window fetched
  for that timeframe. Read-through: on hit-and-fresh it returns cached bars; on
  miss/stale it invokes a fetch func (the alpaca.Client), stores, and returns. TTL is a
  function of timeframe (short for intraday timeframes, longer for `1Day`+). Emits
  cache hit/miss metrics.
- **ranges** — pure logic, no I/O. Maps a range string to `{timeframe, lookback}` and
  slices a cached series down to the requested window. Fully unit-testable.
- **poller** — every `POLL_INTERVAL`, refreshes each watchlist symbol's active (live)
  timeframes through the store fetch path so served data stays near-real-time. A single
  symbol's failure is logged and never breaks the loop.
- **httpapi** — our own JSON routes. Depends on `store` + `ranges`. Wrapped with
  `otelhttp` and a permissive CORS middleware.
- **observability** — `Setup(ctx, cfg) (shutdown func, error)` initializing the OTel
  `TracerProvider`, `MeterProvider`, and `LoggerProvider` over a shared `Resource`, and
  a helper to run the pprof admin server.

### Data flow

**On-demand request** — `GET /bars?symbol=NVDA&range=1y`:
handler validates → `ranges` maps to `{1Day, 1y lookback}` → `store` lookup by
`(NVDA, 1Day)`; hit & fresh → slice & return; miss/stale → `alpaca.GetBars` → store →
slice & return.

**Watchlist poller** — ticker fires → for each watchlist symbol, refresh its active
timeframes via the store fetch path → cache warmed → later requests hit warm cache.

## 4. HTTP API

Public data API (default `:8080`, `PORT` configurable), JSON, stdlib `net/http`
ServeMux with method+pattern routing (Go 1.22+):

- `GET /healthz` → `200 {"status":"ok"}`.
- `GET /bars?symbol={sym}&range={range}` → core endpoint:
  ```json
  {
    "symbol": "AAPL",
    "range": "1d",
    "timeframe": "5Min",
    "bars": [
      { "t": "2026-07-09T13:30:00Z", "o": 0, "h": 0, "l": 0, "c": 0, "v": 0 }
    ]
  }
  ```
  `range` ∈ `{10m,1h,1d,1w,1mo,1y,5y,all}`. Invalid/missing symbol or range → `400`
  with a clear JSON error message.
- `GET /watchlist` → the configured watchlist symbols (so the future frontend can
  populate its default list).

**CORS:** permissive middleware, allowed origin configurable (`CORS_ORIGIN`, default `*`
for dev), since a separate frontend will call this API.

**Admin server** (default `:6060`, `PPROF_ADDR` configurable, empty disables): hosts
`net/http/pprof` endpoints on a separate port so profiling/debug surfaces are not
exposed alongside the public data API.

## 5. Range → Timeframe Mapping

| Range | Timeframe | Lookback |
|-------|-----------|----------|
| 10m   | 1Min      | 10 minutes |
| 1h    | 1Min      | 1 hour |
| 1d    | 5Min      | 1 day |
| 1w    | 1Hour     | 1 week |
| 1mo   | 1Hour     | 1 month |
| 1y    | 1Day      | 1 year |
| 5y    | 1Week     | 5 years |
| all   | 1Month    | max available |

Because several ranges share a timeframe (e.g. `10m` and `1h` both use `1Min`), a single
cached `(symbol, timeframe)` series can serve multiple ranges by slicing — this is the
efficiency win of keying the store by timeframe.

## 6. Configuration

All via env vars; a `.env.example` documents them and the real `.env` stays gitignored.

| Var | Default | Notes |
|-----|---------|-------|
| `ALPACA_API_KEY` | — | Required. Never logged. |
| `ALPACA_API_SECRET` | — | Required. Never logged. |
| `ALPACA_FEED` | `iex` | `iex` (free) or `sip` (paid). |
| `WATCHLIST` | — | Comma-separated symbols, e.g. `AAPL,TSLA,NVDA`. |
| `POLL_INTERVAL` | `20s` | Watchlist refresh cadence. |
| `PORT` | `8080` | Public data API port. |
| `PPROF_ADDR` | `:6060` | Admin/pprof server; empty disables. |
| `CORS_ORIGIN` | `*` | Allowed CORS origin for the data API. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | If unset → stdout fallback / no exporter. |
| `OTEL_SERVICE_NAME` | `alpaca-playground` | Resource service name. |
| `OTEL_SDK_DISABLED` | `false` | Honored to fully disable OTel. |

Server refuses to start without the Alpaca API key and secret.

## 7. Observability

**Package:** `internal/observability`. `Setup(ctx, cfg)` initializes the OTel
`TracerProvider`, `MeterProvider`, and `LoggerProvider` over a shared `Resource`
(`service.name`, version) and returns one shutdown func that flushes all three. Standard
`OTEL_EXPORTER_OTLP_ENDPOINT` conventions; stdout exporter fallback for local dev when no
endpoint is configured.

- **Logs:** stdlib `log/slog` is the app logging API, bridged to OTel via `otelslog` so
  records export over OTLP and remain human-readable on stderr. Trace/span IDs are
  attached to log records for correlation. Structured fields (symbol, range, timeframe,
  latency). Secrets never logged.
- **Traces:** OTLP exporter. Inbound requests wrapped by `otelhttp` (one span per
  request); outbound Alpaca calls traced via the `otelhttp` transport (child spans nested
  under the request span); custom spans around store fetch and poller ticks.
- **Metrics:** OTLP exporter. Instruments:
  - HTTP server request count + duration by route/status (largely from `otelhttp`).
  - `alpaca.client` request count, duration, error count.
  - `store` cache hit/miss counters and entry count.
  - `poller` tick count, duration, per-symbol error count.
- **pprof:** `net/http/pprof` on the separate admin server (§4).

## 8. Error Handling

- Alpaca non-2xx / network errors → wrapped with context; handler returns `502` with a
  small JSON error body; details logged server-side.
- Alpaca **429 rate limit** → alpaca.Client retries with bounded backoff, then surfaces
  the error. Poller logs and skips the tick rather than crashing.
- Validation errors (bad symbol/range) → `400` with a clear message.
- Poller goroutine wraps per-symbol work so one failure never stops the loop.
- **Graceful shutdown:** `SIGINT`/`SIGTERM` → stop poller, `http.Server.Shutdown` (data +
  admin) with a timeout, then flush OTel providers.

## 9. Testing

- **ranges** — pure unit tests: each range maps to the correct timeframe/lookback;
  slicing returns the correct window; empty/partial series handled.
- **store** — unit tests with a fake fetch func: hit/miss/stale behavior, TTL by
  timeframe, concurrent access under `-race`.
- **alpaca.Client** — tested against an `httptest.Server` returning canned Alpaca JSON:
  asserts auth headers, feed param, bar parsing, pagination, and 429 retry. No real
  network.
- **httpapi** — handler tests via `httptest` with a fake store: status codes, validation,
  JSON shape, CORS headers.
- **observability** — setup tested for wiring/no-panic with a no-op/stdout exporter;
  instrumentation is not asserted on in hot-path unit tests.
- `make test` runs `go test ./... -race`.

## 10. Codegen Cleanup

The oapi-codegen config currently emits `std-http-server: true`, generating server stubs
from Alpaca's *own* spec, which we don't use. Set `std-http-server: false` so only the
client + models we actually use are generated. Re-run `make gen-oapi` (fetches a fresh
spec, since `market-data-api.json` is currently empty) as part of implementation.

## 11. New Dependencies

- `go.opentelemetry.io/otel` + SDK (`otel/sdk`, `sdk/metric`, `sdk/log`).
- OTLP exporters (`exporters/otlp/otlptrace/...`, `otlpmetric/...`, `otlplog/...`) and
  stdout exporters for local dev.
- `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`.
- `go.opentelemetry.io/contrib/bridges/otelslog`.

No third-party HTTP router — stdlib `net/http` ServeMux is sufficient.
