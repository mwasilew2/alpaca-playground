package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

// fakeRepo is an in-package Repository double for policy tests.
type fakeRepo struct {
	mu        sync.Mutex
	bars      map[string][]marketdata.Bar
	intervals map[string][]Interval
	putErr    error // if set, PutBars/PutIntervals return it
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{bars: map[string][]marketdata.Bar{}, intervals: map[string][]Interval{}}
}
func (f *fakeRepo) Bars(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []marketdata.Bar
	for _, b := range f.bars[key(s, tf)] {
		if !b.T.Before(start) && !b.T.After(end) {
			out = append(out, b)
		}
	}
	return out, nil
}
func (f *fakeRepo) Intervals(_ context.Context, s, tf string) ([]Interval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Interval(nil), f.intervals[key(s, tf)]...), nil
}
func (f *fakeRepo) PutBars(_ context.Context, s, tf string, bars []marketdata.Bar) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bars[key(s, tf)] = append(f.bars[key(s, tf)], bars...)
	return nil
}
func (f *fakeRepo) PutIntervals(_ context.Context, s, tf string, iv []Interval) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intervals[key(s, tf)] = append([]Interval(nil), iv...)
	return nil
}
func (f *fakeRepo) Close() error { return nil }

func fixedTTL(d time.Duration) func(string) time.Duration {
	return func(string) time.Duration { return d }
}
func fixedLive(d time.Duration) func(string) time.Duration {
	return func(string) time.Duration { return d }
}

func TestGet_MissThenHit(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return []marketdata.Bar{{T: start.Add(time.Minute)}}, nil
	}
	repo := newFakeRepo()
	st := New(fetch, repo, fixedTTL(time.Hour), fixedLive(2*time.Hour))
	st.now = func() time.Time { return now }

	start := now.Add(-30 * time.Minute)
	if _, err := st.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1 (second Get fully covered & fresh)", calls.Load())
	}
}

func TestGet_RecordsEmptyIntervalNoRefetch(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return nil, nil // market closed: no bars, but the range was fetched
	}
	repo := newFakeRepo()
	st := New(fetch, repo, fixedTTL(time.Hour), fixedLive(2*time.Hour))
	st.now = func() time.Time { return now }
	start := now.Add(-30 * time.Minute)
	_, _ = st.Get(context.Background(), "AAPL", "1Min", start, now)
	_, _ = st.Get(context.Background(), "AAPL", "1Min", start, now)
	if calls.Load() != 1 {
		t.Errorf("empty-but-fetched range refetched: calls=%d", calls.Load())
	}
}

func TestGet_ConcurrentNoDoubleFetch(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []marketdata.Bar{{T: start.Add(time.Minute)}}, nil
	}
	repo := newFakeRepo()
	st := New(fetch, repo, fixedTTL(time.Hour), fixedLive(2*time.Hour))
	st.now = func() time.Time { return now }
	start := now.Add(-30 * time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); st.Get(context.Background(), "AAPL", "1Min", start, now) }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("same-key concurrent Get fetched %d times, want 1", calls.Load())
	}
}

func TestGet_PersistFailureStillServes(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		return []marketdata.Bar{{T: start.Add(time.Minute), C: 7}}, nil
	}
	repo := newFakeRepo()
	repo.putErr = context.DeadlineExceeded // simulate disk failure
	st := New(fetch, repo, fixedTTL(time.Hour), fixedLive(2*time.Hour))
	st.now = func() time.Time { return now }
	start := now.Add(-30 * time.Minute)
	got, err := st.Get(context.Background(), "AAPL", "1Min", start, now)
	if err != nil {
		t.Fatalf("persist failure must not fail the request: %v", err)
	}
	if len(got) != 1 || got[0].C != 7 {
		t.Fatalf("degraded serve wrong: %+v", got)
	}
}
