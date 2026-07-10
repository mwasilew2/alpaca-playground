package observability

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// capturingLogExporter records the trace/span ids stamped on each exported log
// record, read at export time to avoid record aliasing.
type capturingLogExporter struct {
	traceIDs []trace.TraceID
	spanIDs  []trace.SpanID
}

func (e *capturingLogExporter) Export(_ context.Context, recs []sdklog.Record) error {
	for i := range recs {
		e.traceIDs = append(e.traceIDs, recs[i].TraceID())
		e.spanIDs = append(e.spanIDs, recs[i].SpanID())
	}
	return nil
}
func (e *capturingLogExporter) Shutdown(context.Context) error   { return nil }
func (e *capturingLogExporter) ForceFlush(context.Context) error { return nil }

// TestLogsCarryTraceContext proves the slog->OTel bridge stamps a log record with
// the active span's trace/span ids when a context is supplied, and stamps none
// when it isn't — the mechanism that makes logs correlate with traces.
func TestLogsCarryTraceContext(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)

	exp := &capturingLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	otellog.SetLoggerProvider(lp) // must precede NewHandler so the bridge binds to lp
	slog.SetDefault(slog.New(otelslog.NewHandler("test")))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	slog.InfoContext(ctx, "correlated log line")
	span.End()

	if len(exp.traceIDs) == 0 {
		t.Fatal("no log records exported")
	}
	if got := exp.traceIDs[len(exp.traceIDs)-1]; got != span.SpanContext().TraceID() {
		t.Errorf("log trace id = %s, want span's %s", got, span.SpanContext().TraceID())
	}
	if got := exp.spanIDs[len(exp.spanIDs)-1]; got != span.SpanContext().SpanID() {
		t.Errorf("log span id = %s, want %s", got, span.SpanContext().SpanID())
	}

	// A bare (no-context) log must NOT carry a trace id — correlation is
	// context-driven, so the wrong logging call silently loses the link.
	slog.Info("uncorrelated")
	if last := exp.traceIDs[len(exp.traceIDs)-1]; last.IsValid() {
		t.Errorf("bare slog.Info should carry no trace id, got %s", last)
	}
}
