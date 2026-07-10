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

func TestRecordAndCount_NoActiveSpanSafe(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)

	// context.Background() has no active span -> SpanFromContext returns a no-op
	// span. RecordError/CountError must not panic and must still count.
	RecordError(context.Background(), ComponentStore, KindInternal, errors.New("boom"))
	CountError(context.Background(), ComponentPoller, KindUpstream)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	if v := counterValue(rm, "store.errors", KindInternal); v != 1 {
		t.Errorf("store.errors{kind=internal} = %d, want 1", v)
	}
	if v := counterValue(rm, "poller.errors", KindUpstream); v != 1 {
		t.Errorf("poller.errors{kind=upstream} = %d, want 1", v)
	}
}
