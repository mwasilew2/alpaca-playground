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
