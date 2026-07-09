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
