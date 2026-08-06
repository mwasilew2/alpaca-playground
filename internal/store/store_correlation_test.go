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

func TestGet_EmitsCorrelatedSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		return nil, nil
	}
	st := New(fetch, NewMemRepository(), fixedTTL(time.Minute), fixedLive(time.Hour))

	parentCtx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	if _, err := st.Get(parentCtx, "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now()); err != nil {
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
		t.Fatal("no store.Get span recorded")
	}
	if got.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("store.Get not in parent trace")
	}
	if got.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("store.Get parent span id mismatch")
	}
}
