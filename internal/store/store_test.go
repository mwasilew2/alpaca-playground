package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

func fixedTTL(d time.Duration) func(string) time.Duration {
	return func(string) time.Duration { return d }
}

func TestGet_CacheHitSkipsFetch(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return []marketdata.Bar{{T: start}}, nil
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := New(fetch, fixedTTL(time.Minute))
	s.now = func() time.Time { return now }

	start := now.Add(-time.Hour)
	if _, err := s.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1 (second should hit cache)", calls.Load())
	}
}

func TestGet_StaleRefetches(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return nil, nil
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := New(fetch, fixedTTL(time.Minute))
	cur := now
	s.now = func() time.Time { return cur }

	start := now.Add(-time.Hour)
	s.Get(context.Background(), "AAPL", "1Min", start, now)
	cur = now.Add(2 * time.Minute) // advance past TTL
	s.Get(context.Background(), "AAPL", "1Min", start, cur)
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2 (stale refetch)", calls.Load())
	}
}

func TestGet_EarlierStartRefetches(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return nil, nil
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := New(fetch, fixedTTL(time.Hour))
	s.now = func() time.Time { return now }

	s.Get(context.Background(), "AAPL", "1Day", now.Add(-24*time.Hour), now)
	s.Get(context.Background(), "AAPL", "1Day", now.Add(-48*time.Hour), now) // wider window
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2 (coverage miss)", calls.Load())
	}
}

func TestGet_Concurrent(t *testing.T) {
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		return []marketdata.Bar{{T: start}}, nil
	}
	s := New(fetch, fixedTTL(time.Minute))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Get(context.Background(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now())
		}()
	}
	wg.Wait()
}
