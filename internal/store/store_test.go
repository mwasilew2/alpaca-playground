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

// TestGet_RepeatRequestWithMovingEndIsAHit pins the trailing-gap rule end to
// end. httpapi and poller both pass end=time.Now(), so a second request always
// covers a sliver the first did not; without the rule every request was a miss
// and store.cache.hits could never fire.
func TestGet_RepeatRequestWithMovingEndIsAHit(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		calls.Add(1)
		return []marketdata.Bar{{T: start.Add(time.Minute)}}, nil
	}
	st := New(fetch, newFakeRepo(), fixedTTL(time.Hour), fixedLive(2*time.Hour))
	st.now = func() time.Time { return now }

	start := now.Add(-30 * time.Minute)
	if _, err := st.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		now = now.Add(200 * time.Millisecond) // the clock moves between requests
		if _, err := st.Get(context.Background(), "AAPL", "1Min", start, now); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1: 5 requests 200ms apart", calls.Load())
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

// TestGet_IncrementalTailDoesNotBlessStaleLive is the end-to-end regression test
// for the freshness bug. Two earlier Gets at different times build two ADJACENT
// stored intervals with different FetchedAt (an old one and a newer touching one).
// A third Get, by which time the old interval is stale but the newer one is still
// fresh, must re-fetch the stale sub-range — the newer neighbour must NOT bless it
// as fresh. Under the old Coalesce(FetchedAt=max) persistence, the two intervals
// merged into one fresh interval and the stale sub-range was never re-fetched.
func TestGet_IncrementalTailDoesNotBlessStaleLive(t *testing.T) {
	type fr struct{ from, to time.Time }
	var mu sync.Mutex
	var recorded []fr
	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		mu.Lock()
		recorded = append(recorded, fr{start, end})
		mu.Unlock()
		return []marketdata.Bar{{T: start.Add(time.Second)}}, nil
	}
	repo := newFakeRepo()
	// TTL 60s; liveHorizon huge so the whole span stays in the mutable live zone.
	st := New(fetch, repo, fixedTTL(60*time.Second), fixedLive(24*time.Hour))
	var nowT time.Time
	st.now = func() time.Time { return nowT }

	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	p0, p1, p2 := base, base.Add(60*time.Second), base.Add(120*time.Second)

	// Get 1 @ base: fetch [p0,p1], stamped FetchedAt=base.
	nowT = base
	if _, err := st.Get(context.Background(), "AAPL", "1Min", p0, p1); err != nil {
		t.Fatal(err)
	}
	// Get 2 @ base+30s: fetch the adjacent tail [p1,p2], stamped FetchedAt=base+30s.
	// Persistence must keep [p0,p1]@base and [p1,p2]@base+30 DISTINCT.
	nowT = base.Add(30 * time.Second)
	if _, err := st.Get(context.Background(), "AAPL", "1Min", p1, p2); err != nil {
		t.Fatal(err)
	}

	// Get 3 @ base+70s (freshAfter=base+10s): [p0,p1]@base is stale, [p1,p2]@base+30
	// is fresh. The stale sub-range around base+30s must be re-fetched.
	mu.Lock()
	recorded = nil
	mu.Unlock()
	nowT = base.Add(70 * time.Second)
	if _, err := st.Get(context.Background(), "AAPL", "1Min", p0, p2); err != nil {
		t.Fatal(err)
	}

	stale := base.Add(30 * time.Second) // inside the older, now-stale [p0,p1]
	mu.Lock()
	defer mu.Unlock()
	covered := false
	for _, r := range recorded {
		if !r.from.After(stale) && !r.to.Before(stale) {
			covered = true
		}
	}
	if !covered {
		t.Errorf("stale live sub-range at %v was not re-fetched on the 3rd Get; ranges=%v", stale, recorded)
	}
}
