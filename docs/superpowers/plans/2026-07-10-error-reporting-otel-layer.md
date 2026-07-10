# Error Reporting OTel Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers-extended-cc:subagent-driven-development (recommended) or superpowers-extended-cc:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a vendor-neutral OpenTelemetry error-reporting layer — stack traces on exceptions, per-component error counters, and consistent recording across store/alpaca/poller/httpapi — so any OTel-native backend can group and investigate errors.

**Architecture:** A shared helper in `internal/observability` exposes `RecordError` (exception event + stack trace + Error status + per-component counter, called once at a failure's origin) and `CountError` (counter only, for layers surfacing an already-recorded error and for expected 400s). Call sites are wired so the exception is recorded exactly once (at the alpaca origin) while each layer keeps its own `<component>.errors{kind}` counter.

**Tech Stack:** Go 1.26, OpenTelemetry Go SDK (traces + metrics), `trace.WithStackTrace`, existing otelhttp/otelslog wiring.

**User decisions (already made):**
- Shared helper (not inline per-site recording).
- Stack traces always on recorded server-side errors (`trace.WithStackTrace(true)`).
- Per-component counters (`<component>.errors{kind}`), NOT a unified `errors.total`.
- Per-layer counter overlap on one failure is acceptable (distinct SLIs).
- 400 validation errors: counted with `kind=validation`, no span error status, no stack trace.
- Out of scope (deferred): Sentry/Bugsnag SDK, unified metric, standing up SigNoz.

---

## File Structure

```
internal/observability/errors.go        # NEW: RecordError, CountError, component/kind consts
internal/observability/errors_test.go   # NEW: unit tests (SpanRecorder + ManualReader)
internal/alpaca/client.go               # MODIFY: RecordError at the two GetBars error sites + kind classifier
internal/store/store.go                 # MODIFY: drop span.RecordError (keep SetStatus)
internal/poller/poller.go               # MODIFY: drop errs counter; SetStatus + CountError on refresh error
internal/httpapi/handlers.go            # MODIFY: CountError on 502 and the two 400 paths
```

**Cross-task contract (define in Task 1, reuse verbatim):**

```go
// internal/observability/errors.go
const (
	ComponentStore  = "store"
	ComponentAlpaca = "alpaca"
	ComponentPoller = "poller"
	ComponentHTTP   = "httpapi"
)
const (
	KindUpstream    = "upstream"
	KindRateLimited = "rate_limited"
	KindTimeout     = "timeout"
	KindValidation  = "validation"
	KindInternal    = "internal"
)
func RecordError(ctx context.Context, component, kind string, err error) // exception+stacktrace+status+counter (origin)
func CountError(ctx context.Context, component, kind string)             // counter only
```

No import cycles: `observability` imports only `config`; the other packages import `observability`.

---

### Task 1: Error helper (`internal/observability/errors.go`)

**Goal:** Implement `RecordError` and `CountError` plus the component/kind constants, with unit tests proving stack-trace capture, span status, and per-component counters.

**Files:**
- Create: `internal/observability/errors.go`
- Create: `internal/observability/errors_test.go`

**Acceptance Criteria:**
- [ ] `RecordError(ctx, "alpaca", "upstream", err)` records an `exception` span event whose `exception.stacktrace` attribute is non-empty, sets span status `Error`, and increments `alpaca.errors{kind=upstream}` by 1.
- [ ] `CountError(ctx, "httpapi", "validation")` increments `httpapi.errors{kind=validation}` by 1, leaves span status `Unset`, and records no exception event.
- [ ] Counter instrument is resolved from the current global meter on each call (no stale caching across MeterProviders).

**Verify:** `go test ./internal/observability/... -race -v` → PASS

**Steps:**

- [ ] **Step 1: Write the failing test**

```go
// internal/observability/errors_test.go
package observability

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// counterValue returns the sum data point value for instrument `name` whose
// data point carries attribute kind=`kind`, or -1 if not found.
func counterValue(rm metricdata.ResourceMetrics, name, kind string) int64 {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, present := dp.Attributes.Value(attribute.Key("kind")); present && v.AsString() == kind {
					return dp.Value
				}
			}
		}
	}
	return -1
}

func exceptionStacktrace(sp sdktrace.ReadOnlySpan) (string, bool) {
	for _, ev := range sp.Events() {
		if ev.Name != "exception" {
			continue
		}
		for _, a := range ev.Attributes {
			if a.Key == "exception.stacktrace" {
				return a.Value.AsString(), true
			}
		}
	}
	return "", false
}

func TestRecordError_StacktraceStatusAndCounter(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)

	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	RecordError(ctx, ComponentAlpaca, KindUpstream, errors.New("boom"))
	span.End()

	got := sr.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", got.Status().Code)
	}
	st, ok := exceptionStacktrace(got)
	if !ok || st == "" {
		t.Errorf("expected non-empty exception.stacktrace, got ok=%v %q", ok, st)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	if v := counterValue(rm, "alpaca.errors", KindUpstream); v != 1 {
		t.Errorf("alpaca.errors{kind=upstream} = %d, want 1", v)
	}
}

func TestCountError_CounterOnlyNoSpanChange(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)

	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	CountError(ctx, ComponentHTTP, KindValidation)
	span.End()

	got := sr.Ended()[0]
	if got.Status().Code == codes.Error {
		t.Error("CountError must not set span status to Error")
	}
	if _, ok := exceptionStacktrace(got); ok {
		t.Error("CountError must not record an exception event")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	if v := counterValue(rm, "httpapi.errors", KindValidation); v != 1 {
		t.Errorf("httpapi.errors{kind=validation} = %d, want 1", v)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/observability/... -run 'RecordError|CountError' -v`
Expected: FAIL (undefined `RecordError`, `CountError`, `ComponentAlpaca`, …).

- [ ] **Step 3: Implement the helper**

```go
// internal/observability/errors.go
package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Component identifiers for error attribution (the "<component>" in
// "<component>.errors").
const (
	ComponentStore  = "store"
	ComponentAlpaca = "alpaca"
	ComponentPoller = "poller"
	ComponentHTTP   = "httpapi"
)

// Kind classifies an error for grouping/alerting (the "kind" attribute).
const (
	KindUpstream    = "upstream"
	KindRateLimited = "rate_limited"
	KindTimeout     = "timeout"
	KindValidation  = "validation"
	KindInternal    = "internal"
)

// addErrorMetric increments <component>.errors{kind}. The instrument is resolved
// from the current global meter on each call (the SDK returns the same
// instrument for a given name), so there is no stale caching across providers.
func addErrorMetric(ctx context.Context, component, kind string) {
	c, err := otel.Meter("alpaca-playground/errors").Int64Counter(component + ".errors")
	if err != nil {
		return
	}
	c.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}

// RecordError records a server-side fault at its origin: on the active span it
// records the exception WITH a stack trace and sets status Error, then
// increments <component>.errors{kind}. Call it exactly once per failure, at the
// point the failure originates, so nested spans don't accumulate duplicate
// exception events. It never alters err or control flow.
func RecordError(ctx context.Context, component, kind string, err error) {
	span := trace.SpanFromContext(ctx)
	span.RecordError(err, trace.WithStackTrace(true))
	span.SetStatus(codes.Error, err.Error())
	addErrorMetric(ctx, component, kind)
}

// CountError increments <component>.errors{kind} only — it does not touch the
// span or record an exception. Use it at layers surfacing an error already
// recorded downstream, and for expected client errors (e.g. HTTP 400).
func CountError(ctx context.Context, component, kind string) {
	addErrorMetric(ctx, component, kind)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/observability/... -race -v`
Expected: PASS (existing observability tests plus the two new ones).

- [ ] **Step 5: Tidy + commit**

```bash
go mod tidy
git add internal/observability/errors.go internal/observability/errors_test.go go.mod go.sum
git commit --no-gpg-sign -m "feat(observability): add RecordError/CountError error helper"
```

```json:metadata
{"files": ["internal/observability/errors.go", "internal/observability/errors_test.go"], "verifyCommand": "go test ./internal/observability/... -race -v", "acceptanceCriteria": ["RecordError records exception with non-empty stacktrace + Error status + alpaca.errors{kind} counter", "CountError increments counter only, no span change/exception", "counter resolved from current global meter (no stale cache)"], "modelTier": "standard"}
```

---

### Task 2: Wire alpaca (origin) and store to the helper

**Goal:** Record the upstream exception + stack trace + `alpaca.errors{kind}` once at the alpaca origin, and remove store's now-duplicate `span.RecordError` (keeping its span status).

**Files:**
- Modify: `internal/alpaca/client.go`
- Modify: `internal/store/store.go`

**Acceptance Criteria:**
- [ ] `alpaca.GetBars` calls `observability.RecordError(ctx, ComponentAlpaca, kind, err)` at both error sites (transport error and unexpected status); `kind` = `rate_limited` when the final status is 429, `timeout` when the error is `context.DeadlineExceeded`, else `upstream`.
- [ ] `client.go` no longer calls `span.RecordError`/`span.SetStatus` directly (the helper does both) and no longer imports `codes`.
- [ ] `store.Get` keeps `span.SetStatus(codes.Error, "upstream fetch failed")` but no longer calls `span.RecordError` (no duplicate exception; alpaca owns it).
- [ ] Existing alpaca and store tests still pass unchanged.

**Verify:** `go test ./internal/alpaca/... ./internal/store/... -race` → PASS

**Steps:**

- [ ] **Step 1: Update `internal/alpaca/client.go` imports**

Replace the import block's OTel section — drop `codes`, add nothing new besides `observability`, `errors`, `context` (already present). Final relevant imports:

```go
import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mwasilew2/alpaca-playground/gen/oapi"
	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/observability"

	"github.com/hashicorp/go-retryablehttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)
```

(`errors` is newly used by the kind classifier; `codes` is removed.)

- [ ] **Step 2: Add a kind classifier and replace the two error sites in `GetBars`**

Replace the current loop body error handling. The current code is:

```go
		resp, err := c.doWithRetry(ctx, params)
```

— note: after the retryablehttp refactor there is no `doWithRetry`; the call is `resp, err := c.oc.StockBarsWithResponse(ctx, params)`. Update the two error branches to:

```go
		resp, err := c.oc.StockBarsWithResponse(ctx, params)
		if err != nil {
			kind := observability.KindUpstream
			if errors.Is(err, context.DeadlineExceeded) {
				kind = observability.KindTimeout
			}
			observability.RecordError(ctx, observability.ComponentAlpaca, kind, err)
			return nil, err
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			err := fmt.Errorf("alpaca stock bars: unexpected status %d", resp.StatusCode())
			kind := observability.KindUpstream
			if resp.StatusCode() == http.StatusTooManyRequests {
				kind = observability.KindRateLimited
			}
			observability.RecordError(ctx, observability.ComponentAlpaca, kind, err)
			return nil, err
		}
```

This removes the previous `span.RecordError(err)` / `span.SetStatus(codes.Error, …)` pairs (the helper performs both on the active `alpaca.GetBars` span). Leave the span start (`c.tracer.Start`), the `bars.count` attribute, pagination, and `convert` unchanged.

- [ ] **Step 3: Update `internal/store/store.go`**

In `Get`, the fetch-error branch currently reads:

```go
	bars, err := s.fetch(ctx, symbol, timeframe, start, end)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream fetch failed")
		return nil, err
	}
```

Remove the `span.RecordError(err)` line only:

```go
	bars, err := s.fetch(ctx, symbol, timeframe, start, end)
	if err != nil {
		span.SetStatus(codes.Error, "upstream fetch failed")
		return nil, err
	}
```

`store.go` keeps its `codes` import (still used by `SetStatus`). Do NOT add an error counter here.

- [ ] **Step 4: Verify build + tests**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./internal/alpaca/... ./internal/store/... -race`
Expected: build/vet clean, gofmt empty, alpaca + store tests PASS (they assert returned errors/statuses/behavior, which are unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/alpaca/client.go internal/store/store.go
git commit --no-gpg-sign -m "feat(alpaca,store): record upstream errors via observability helper"
```

```json:metadata
{"files": ["internal/alpaca/client.go", "internal/store/store.go"], "verifyCommand": "go build ./... && go test ./internal/alpaca/... ./internal/store/... -race", "acceptanceCriteria": ["alpaca RecordError at both error sites with kind rate_limited/timeout/upstream", "client.go no longer imports codes / calls span.RecordError directly", "store.Get keeps SetStatus, drops span.RecordError", "existing alpaca+store tests pass"], "modelTier": "mechanical"}
```

---

### Task 3: Wire poller and httpapi to the helper

**Goal:** Give the poller and HTTP handler their own `<component>.errors{kind}` counters via `CountError` (no duplicate exception events), remove the poller's bespoke `errs` counter, and count 400 validation errors.

**Files:**
- Modify: `internal/poller/poller.go`
- Modify: `internal/httpapi/handlers.go`

**Acceptance Criteria:**
- [ ] `poller` no longer has an `errs metric.Int64Counter` field; on a refresh error it calls `span.SetStatus(codes.Error, "refresh error")` on the refresh span and `observability.CountError(ctx, ComponentPoller, KindUpstream)`, and keeps the `WarnContext` log.
- [ ] `httpapi.handleBars` calls `observability.CountError(ctx, ComponentHTTP, KindUpstream)` on the 502 path (keeping the `ErrorContext` log) and `observability.CountError(ctx, ComponentHTTP, KindValidation)` on both 400 paths.
- [ ] Existing poller and httpapi tests still pass unchanged.
- [ ] `make otel-smoke` shows `exception.stacktrace` on the `alpaca.GetBars` span and `alpaca.errors` / `httpapi.errors` / `poller.errors` counters in the metric dump.

**Verify:** `go test ./internal/poller/... ./internal/httpapi/... -race` → PASS

**Steps:**

- [ ] **Step 1: Update `internal/poller/poller.go`**

Remove the `errs` field and its construction; add `codes` + `observability` imports. The struct becomes:

```go
type Poller struct {
	store      *store.Store
	symbols    []string
	timeframes []string
	interval   time.Duration
	now        func() time.Time

	tracer trace.Tracer
	ticks  metric.Int64Counter
}
```

In `New`, delete the `errs` counter creation and field assignment (keep `ticks`):

```go
func New(s *store.Store, symbols, timeframes []string, interval time.Duration) *Poller {
	m := otel.Meter("alpaca-playground/poller")
	ticks, _ := m.Int64Counter("poller.ticks")
	return &Poller{
		store:      s,
		symbols:    symbols,
		timeframes: timeframes,
		interval:   interval,
		now:        time.Now,
		tracer:     otel.Tracer("alpaca-playground/poller"),
		ticks:      ticks,
	}
}
```

In `refresh`, replace the error handling inside the loop:

```go
			if _, err := p.store.Get(ctx, sym, tf, start, now); err != nil {
				span.SetStatus(codes.Error, "refresh error")
				observability.CountError(ctx, observability.ComponentPoller, observability.KindUpstream)
				slog.WarnContext(ctx, "poller refresh failed", "symbol", sym, "timeframe", tf, "err", err)
			}
```

Add imports: `"go.opentelemetry.io/otel/codes"` and `"github.com/mwasilew2/alpaca-playground/internal/observability"`. Keep the `metric` import (still used by `ticks`).

- [ ] **Step 2: Update `internal/httpapi/handlers.go`**

Add `"github.com/mwasilew2/alpaca-playground/internal/observability"` to imports. In `handleBars`:

The missing-symbol 400:

```go
	if symbol == "" {
		observability.CountError(r.Context(), observability.ComponentHTTP, observability.KindValidation)
		writeError(w, http.StatusBadRequest, "missing required query param: symbol")
		return
	}
```

The invalid-range 400:

```go
	spec, err := ranges.Resolve(rng)
	if err != nil {
		observability.CountError(r.Context(), observability.ComponentHTTP, observability.KindValidation)
		writeError(w, http.StatusBadRequest, "invalid range: "+rangeStr)
		return
	}
```

The upstream 502:

```go
	bars, err := s.src.Get(r.Context(), symbol, spec.Timeframe, start, now)
	if err != nil {
		slog.ErrorContext(r.Context(), "bars fetch failed", "symbol", symbol, "range", rangeStr, "err", err)
		observability.CountError(r.Context(), observability.ComponentHTTP, observability.KindUpstream)
		writeError(w, http.StatusBadGateway, "upstream market data error")
		return
	}
```

- [ ] **Step 3: Verify build + full test suite**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -race`
Expected: build/vet clean, gofmt empty, all packages PASS. Poller tests count fetch calls (not the removed `errs` field) and httpapi tests assert responses/CORS — both unchanged by this task.

- [ ] **Step 4: Confirm end-to-end telemetry**

Run: `make otel-smoke`
Expected: in the metric dump, counters `alpaca.errors`, `httpapi.errors`, and `poller.errors` appear with a `kind` attribute; in the trace output, the `alpaca.GetBars` span has an `exception` event carrying a non-empty `exception.stacktrace`. (The script's request path 502s against the stub, exercising all three.)

- [ ] **Step 5: Commit**

```bash
git add internal/poller/poller.go internal/httpapi/handlers.go
git commit --no-gpg-sign -m "feat(poller,httpapi): count errors via observability helper"
```

```json:metadata
{"files": ["internal/poller/poller.go", "internal/httpapi/handlers.go"], "verifyCommand": "go build ./... && go test ./... -race", "acceptanceCriteria": ["poller drops errs field; refresh error sets span status + CountError(poller,upstream) + WarnContext", "handleBars CountError(httpapi,upstream) on 502 and CountError(httpapi,validation) on both 400s", "existing poller+httpapi tests pass", "make otel-smoke shows exception.stacktrace + *.errors counters"], "modelTier": "mechanical"}
```

---

## Self-Review

**Spec coverage:**
- §3 helper (`RecordError`/`CountError`, per-component counters, constants, stack trace, nil-guards) → Task 1. ✓
- §4 recording model: alpaca origin `RecordError` + kind classifier → Task 2; store `SetStatus`-only (drop RecordError) → Task 2; poller `SetStatus`+`CountError`, drop `errs` → Task 3; httpapi `CountError` on 502 + both 400s → Task 3; main.go unchanged → (no task, explicitly). ✓
- §6 testing: observability unit tests → Task 1; existing-tests-still-pass → Tasks 2/3 verify; `make otel-smoke` end-to-end → Task 3 Step 4. ✓
- §7 out-of-scope (no SDK/backend/unified metric) → nothing in the plan adds them. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code. ✓

**Type consistency:** `RecordError(ctx, component, kind, err)` and `CountError(ctx, component, kind)` signatures used identically in Tasks 2–3. Constants `ComponentAlpaca/Store/Poller/HTTP` and `KindUpstream/RateLimited/Timeout/Validation` referenced exactly as defined in Task 1. `store.Get` retains `codes` (SetStatus); `client.go` drops `codes`; `poller` adds `codes`. ✓

**Note on a subtlety:** Task 2 references the post-retryablehttp call `c.oc.StockBarsWithResponse(ctx, params)` (there is no `doWithRetry` after the retryablehttp merge) — the step calls this out explicitly so the implementer edits the right lines.
