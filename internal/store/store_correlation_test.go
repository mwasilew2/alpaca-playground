package store

import (
	"context"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestGet_EmitsCorrelatedSpan proves store.Get emits a span that nests under the
// caller's span (same trace, correct parent) and carries the cache.hit attribute
// — i.e. store work is traceable and correlated with the request that triggered it.
func TestGet_EmitsCorrelatedSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		return nil, nil
	}
	// Construct AFTER installing the provider so the store picks up our tracer.
	s := New(fetch, fixedTTL(time.Minute))

	parentCtx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	if _, err := s.Get(parentCtx, "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	parent.End()

	var got sdktrace.ReadOnlySpan
	for _, sp := range sr.Ended() {
		if sp.Name() == "store.Get" {
			got = sp
		}
	}
	if got == nil {
		t.Fatal("no store.Get span was recorded")
	}
	if got.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("store.Get trace id = %s, want parent's %s",
			got.SpanContext().TraceID(), parent.SpanContext().TraceID())
	}
	if got.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("store.Get parent span id = %s, want %s",
			got.Parent().SpanID(), parent.SpanContext().SpanID())
	}
	hasHit := false
	for _, a := range got.Attributes() {
		if a.Key == "cache.hit" {
			hasHit = true
		}
	}
	if !hasHit {
		t.Error("store.Get span missing cache.hit attribute")
	}
}
