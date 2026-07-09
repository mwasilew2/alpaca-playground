package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

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
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
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

	// Construct all fallible exporters first so that no provider (and its
	// background goroutine) is created and installed as a global before we
	// know every exporter can be built successfully.

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

	// Metrics
	var metricExp sdkmetric.Exporter
	if useOTLP {
		metricExp, err = otlpmetrichttp.New(ctx)
	} else {
		metricExp, err = stdoutmetric.New()
	}
	if err != nil {
		return nil, err
	}

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

	// All exporters succeeded; now construct providers and install globals.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(spanExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)

	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)), sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)

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
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
