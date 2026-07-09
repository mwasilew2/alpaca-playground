package alpaca

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points the client at ts and uses a fast (near-zero) backoff.
func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	c, err := New(ts.URL, "key-123", "secret-456", "iex", ts.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.backoff = func(int) time.Duration { return time.Millisecond }
	c.maxRetries = 3
	return c
}

func TestGetBars_AuthFeedAndPagination(t *testing.T) {
	var gotKey, gotSecret, gotFeed string
	page := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("APCA-API-KEY-ID")
		gotSecret = r.Header.Get("APCA-API-SECRET-KEY")
		gotFeed = r.URL.Query().Get("feed")
		w.Header().Set("Content-Type", "application/json")
		if page == 0 {
			page++
			json.NewEncoder(w).Encode(map[string]any{
				"bars": map[string]any{"AAPL": []map[string]any{
					{"t": "2026-07-09T13:30:00Z", "o": 1, "h": 2, "l": 0.5, "c": 1.5, "v": 100, "n": 10, "vw": 1.2},
				}},
				"next_page_token": "TOKEN2",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"bars": map[string]any{"AAPL": []map[string]any{
				{"t": "2026-07-09T13:31:00Z", "o": 1.5, "h": 2.5, "l": 1, "c": 2, "v": 200, "n": 20, "vw": 1.8},
			}},
			"next_page_token": nil,
		})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	start := time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	bars, err := c.GetBars(context.Background(), "AAPL", "1Min", start, end)
	if err != nil {
		t.Fatalf("GetBars: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2 (pagination)", len(bars))
	}
	if bars[0].C != 1.5 || bars[1].V != 200 {
		t.Errorf("conversion wrong: %+v", bars)
	}
	if gotKey != "key-123" || gotSecret != "secret-456" || gotFeed != "iex" {
		t.Errorf("auth/feed not sent: key=%q secret=%q feed=%q", gotKey, gotSecret, gotFeed)
	}
}

func TestGetBars_RetriesOn429(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"bars":            map[string]any{"AAPL": []map[string]any{{"t": "2026-07-09T13:30:00Z", "c": 1}}},
			"next_page_token": nil,
		})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	bars, err := c.GetBars(context.Background(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if len(bars) != 1 || calls.Load() != 3 {
		t.Errorf("calls=%d bars=%d, want 3 calls and 1 bar", calls.Load(), len(bars))
	}
}

func TestGetBars_ErrorsOn500(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	_, err := c.GetBars(context.Background(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500, got: %v", err)
	}
}
