package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

// fakeStore implements the barSource interface used by the server.
type fakeStore struct {
	bars []marketdata.Bar
	err  error
}

// Get mimics the real Store's contract: only bars within [start,end] are
// returned (the store now owns that filtering; the handler no longer slices).
func (f fakeStore) Get(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []marketdata.Bar
	for _, b := range f.bars {
		if !b.T.Before(start) && !b.T.After(end) {
			out = append(out, b)
		}
	}
	return out, nil
}

func newTestServer(src barSource) http.Handler {
	s := New(src, []string{"AAPL", "TSLA"}, "*")
	s.now = func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }
	return s.Handler()
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(fakeStore{}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() == "" {
		t.Fatalf("healthz: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestBars_OK(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	src := fakeStore{bars: []marketdata.Bar{
		{T: now.Add(-48 * time.Hour)}, // older than 1d window -> sliced out
		{T: now.Add(-1 * time.Hour)},  // within 1d window
	}}
	rec := httptest.NewRecorder()
	newTestServer(src).ServeHTTP(rec, httptest.NewRequest("GET", "/bars?symbol=AAPL&range=1d", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Symbol    string           `json:"symbol"`
		Range     string           `json:"range"`
		Timeframe string           `json:"timeframe"`
		Bars      []marketdata.Bar `json:"bars"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Symbol != "AAPL" || resp.Range != "1d" || resp.Timeframe != "5Min" {
		t.Errorf("bad envelope: %+v", resp)
	}
	if len(resp.Bars) != 1 {
		t.Errorf("bars=%d, want 1 after slicing", len(resp.Bars))
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
}

func TestBars_Validation(t *testing.T) {
	for _, q := range []string{"/bars", "/bars?symbol=AAPL", "/bars?symbol=AAPL&range=bogus", "/bars?range=1d"} {
		rec := httptest.NewRecorder()
		newTestServer(fakeStore{}).ServeHTTP(rec, httptest.NewRequest("GET", q, nil))
		if rec.Code != 400 {
			t.Errorf("%s: code=%d, want 400", q, rec.Code)
		}
	}
}

func TestBars_UpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	src := fakeStore{err: errors.New("boom")}
	newTestServer(src).ServeHTTP(rec, httptest.NewRequest("GET", "/bars?symbol=AAPL&range=1d", nil))
	if rec.Code != 502 {
		t.Errorf("code=%d, want 502", rec.Code)
	}
}

func TestWatchlist(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(fakeStore{}).ServeHTTP(rec, httptest.NewRequest("GET", "/watchlist", nil))
	var syms []string
	json.Unmarshal(rec.Body.Bytes(), &syms)
	if len(syms) != 2 || syms[0] != "AAPL" {
		t.Errorf("watchlist = %v", syms)
	}
}

func TestCORSHeaderOnErrorResponse(t *testing.T) {
	// Spec: CORS header must be present on ALL responses, including errors.
	rec := httptest.NewRecorder()
	src := fakeStore{err: errors.New("boom")}
	newTestServer(src).ServeHTTP(rec, httptest.NewRequest("GET", "/bars?symbol=AAPL&range=1d", nil))
	if rec.Code != 502 {
		t.Fatalf("code=%d, want 502", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header missing on 502 error response")
	}

	// Also assert on a 400 validation error.
	rec2 := httptest.NewRecorder()
	newTestServer(fakeStore{}).ServeHTTP(rec2, httptest.NewRequest("GET", "/bars?symbol=AAPL&range=bogus", nil))
	if rec2.Code != 400 {
		t.Fatalf("code=%d, want 400", rec2.Code)
	}
	if rec2.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header missing on 400 error response")
	}
}

func TestOptionsPreflight(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(fakeStore{}).ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/bars", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS code=%d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header missing on OPTIONS preflight")
	}
}
