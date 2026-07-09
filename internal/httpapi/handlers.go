package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/ranges"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// barSource is the store dependency the API needs (satisfied by *store.Store).
type barSource interface {
	Get(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)
}

// Server holds the API dependencies.
type Server struct {
	src        barSource
	watchlist  []string
	corsOrigin string
	now        func() time.Time
}

// New builds the API server.
func New(src barSource, watchlist []string, corsOrigin string) *Server {
	return &Server{src: src, watchlist: watchlist, corsOrigin: corsOrigin, now: time.Now}
}

// Handler returns the fully-wrapped HTTP handler (routes + CORS + otelhttp).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /bars", s.handleBars)
	mux.HandleFunc("GET /watchlist", s.handleWatchlist)
	return otelhttp.NewHandler(s.cors(mux), "http.server")
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleWatchlist(w http.ResponseWriter, r *http.Request) {
	syms := s.watchlist
	if syms == nil {
		syms = []string{}
	}
	writeJSON(w, http.StatusOK, syms)
}

type barsResponse struct {
	Symbol    string           `json:"symbol"`
	Range     string           `json:"range"`
	Timeframe string           `json:"timeframe"`
	Bars      []marketdata.Bar `json:"bars"`
}

func (s *Server) handleBars(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	rangeStr := r.URL.Query().Get("range")
	if symbol == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: symbol")
		return
	}
	rng := ranges.Range(rangeStr)
	spec, err := ranges.Resolve(rng)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid range: "+rangeStr)
		return
	}

	now := s.now()
	start := spec.Start(now)
	bars, err := s.src.Get(r.Context(), symbol, spec.Timeframe, start, now)
	if err != nil {
		slog.Error("bars fetch failed", "symbol", symbol, "range", rangeStr, "err", err)
		writeError(w, http.StatusBadGateway, "upstream market data error")
		return
	}

	out := ranges.Slice(bars, start)
	if out == nil {
		out = []marketdata.Bar{}
	}
	writeJSON(w, http.StatusOK, barsResponse{
		Symbol:    symbol,
		Range:     rangeStr,
		Timeframe: spec.Timeframe,
		Bars:      out,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
