# Error Reporting OTel Layer — Design

**Date:** 2026-07-10
**Status:** Approved (design phase)
**Scope:** Vendor-neutral OpenTelemetry error reporting only. NO error-tracking SDK
(Sentry/Bugsnag) and NO backend (SigNoz/etc.) — those are deferred until an
observability backend is chosen. See `memory/error-tracking-decision-paused`.

## 1. Purpose

Close the gaps in how the alpaca backend reports errors to OpenTelemetry so that
any OTel-native backend (SigNoz, Uptrace, Datadog, …) can group and investigate
them. Today errors surface as span status + exception events (store, alpaca),
trace-stamped error logs (httpapi, poller), and a single `poller.errors` counter,
but there are **no stack traces**, **no per-component error metric**, and **400s
are uncounted**. This spec adds a shared error-recording helper that fills those
gaps consistently.

## 2. Established Decisions

- **Shared helper** centralizes error recording (DRY, consistent) rather than
  inline recording at each site.
- **Stack traces always on** recorded (server-side) errors via
  `trace.WithStackTrace(true)`.
- **Per-component counters** (`<component>.errors{kind}`), not a single unified
  `errors.total`. Per-layer overlap on one failure is accepted (distinct SLIs).
- **400 validation errors** are counted (`kind=validation`) but do NOT set span
  error status and carry no stack trace — 4xx is expected client input.

## 3. The Helper (`internal/observability/errors.go`)

A recorder owning per-component `Int64Counter`s, created lazily and cached behind
a mutex, from `otel.Meter("alpaca-playground/errors")`. Counter name is
`<component>.errors`; `kind` is an attribute.

```go
// RecordError records a server-side fault at its origin: on the active span it
// records the exception WITH a stack trace and sets status Error, and increments
// <component>.errors{kind}. Use it exactly once per failure, where the failure
// originates, so nested spans don't accumulate duplicate exception events.
func RecordError(ctx context.Context, component, kind string, err error)

// CountError increments <component>.errors{kind} only — it does NOT touch span
// status or record an exception/stack trace. Use it at layers that surface or
// handle an error already recorded downstream (so they get their own counter
// without duplicating the exception event), and for expected client errors
// (e.g. HTTP 400).
func CountError(ctx context.Context, component, kind string)
```

Exported constants:

- Components: `ComponentStore = "store"`, `ComponentAlpaca = "alpaca"`,
  `ComponentPoller = "poller"`, `ComponentHTTP = "httpapi"`.
- Kinds: `KindUpstream = "upstream"`, `KindRateLimited = "rate_limited"`,
  `KindTimeout = "timeout"`, `KindValidation = "validation"`,
  `KindInternal = "internal"`.

`RecordError` uses `trace.SpanFromContext(ctx)`; if there is no recording span
(e.g. no active trace) the span calls are no-ops and only the counter moves.
Counters are nil-guarded like the existing store/poller instruments.

## 4. Recording Model — where each thing happens

**Rule:** record the exception event + stack trace exactly once, at the origin of
the failure, so nested spans don't duplicate exception events. Counters stay
per-component; a single failure may increment more than one component's counter
(intentional — different SLIs).

| Site | Change |
|------|--------|
| `alpaca.GetBars` (origin of upstream failures) | Replace the manual `RecordError`/`SetStatus` calls with `observability.RecordError(ctx, ComponentAlpaca, kind, err)` → exception+stacktrace+status on the alpaca span + `alpaca.errors{kind}`. `kind`: final HTTP 429 → `rate_limited`; `errors.Is(err, context.DeadlineExceeded)` → `timeout`; otherwise `upstream`. This is the **only** exception-event recording on the request path. |
| `store.Get` | On fetch error, keep `span.SetStatus(codes.Error, …)` inline but **remove** the current `span.RecordError(err)` line (the alpaca child already carries the exception+stacktrace — this deletes today's duplicate). No store counter (its only failure mode is the upstream fetch). |
| `poller.refresh` | Keep the `WarnContext` log. On a `store.Get` error, set the poller.refresh span status inline (`span.SetStatus(codes.Error, …)`) and call `observability.CountError(ctx, ComponentPoller, KindUpstream)` → `poller.errors{kind}` (failed-tick SLI). Remove the poller's own `errs` counter field (the helper owns it now); no exception event (already at alpaca). |
| `httpapi.handleBars` | Keep the `ErrorContext` log on 502 and call `observability.CountError(ctx, ComponentHTTP, KindUpstream)` → `httpapi.errors{kind=upstream}` (the http.server span is already 5xx via otelhttp; no duplicate exception event). On the 400 paths (missing symbol / invalid range) → `observability.CountError(ctx, ComponentHTTP, KindValidation)`. |
| `main.go` startup / server-failure `slog.Error` | Unchanged — outside any trace, not a per-request error. |

### Resulting counters

- `alpaca.errors{kind=upstream|rate_limited|timeout}` — Alpaca call failures.
- `poller.errors{kind=upstream}` — failed refresh ticks.
- `httpapi.errors{kind=upstream|validation}` — request failures surfaced to
  clients (validation = expected 400s).
- `store` has no error counter (failures are upstream, counted at alpaca).

### Resulting traces

A failed `/bars` request shows: `GET /bars` (5xx via otelhttp) → `store.Get`
(status Error) → `alpaca.GetBars` (status Error + exception event **with
`exception.stacktrace`**) → outbound `HTTP GET`. The poller path is the same
under a `poller.refresh` root span. Logs (`ErrorContext`/`WarnContext`) remain
trace-stamped, so the log, the exception event, and the `*.errors` exemplar all
share the trace id.

## 5. Error Handling (of the layer itself)

The helper must never panic or change control flow: nil-guard counters, tolerate
a non-recording span, and never wrap/alter the caller's `err` (callers still
return/handle it exactly as before). Counter creation failure (unlikely) yields a
nil counter that is skipped.

## 6. Testing

- **observability** unit tests (new): with an in-memory `SpanRecorder` +
  `MeterProvider` reader,
  - `RecordError` produces one exception event carrying a non-empty
    `exception.stacktrace`, sets span status Error, and increments
    `<component>.errors` with the `kind` attribute.
  - `CountError` increments the counter but leaves span status **unset** and
    records **no** exception event.
- **store/alpaca/poller/httpapi**: existing tests updated where manual recording
  moved to the helper; behavior is unchanged (errors still returned, statuses
  still set, retry/pagination untouched). Poller test no longer asserts on the
  removed `errs` field — it drives errors through the helper path instead.
- **End-to-end**: `make otel-smoke` now shows `exception.stacktrace` on the
  `alpaca.GetBars` span and the `<component>.errors` counters in the metric dump.
- `make test` runs `go test ./... -race`.

## 7. Out of Scope (deferred)

- Sentry/Bugsnag or any error-tracking SDK.
- A unified `errors.total` metric.
- Standing up SigNoz/Uptrace or any backend.
- Recording `main.go` lifecycle errors as per-request errors.
