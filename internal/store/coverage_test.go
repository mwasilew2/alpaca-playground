package store

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// at treats d as a count of seconds relative to base (not raw nanoseconds), so
// small int offsets like at(10000) stay comparable to ttl/liveHorizon values
// such as 100*time.Second used throughout these tests.
func at(d time.Duration) time.Time { return base.Add(d * time.Second) }

func TestCoalesce(t *testing.T) {
	in := []Interval{
		{From: at(10), To: at(20), FetchedAt: at(100)},
		{From: at(15), To: at(25), FetchedAt: at(200)}, // overlaps -> merge, max fetchedAt
		{From: at(25), To: at(30), FetchedAt: at(50)},  // touches -> merge
		{From: at(40), To: at(50), FetchedAt: at(60)},  // disjoint
	}
	got := Coalesce(in)
	if len(got) != 2 {
		t.Fatalf("got %d intervals, want 2: %+v", len(got), got)
	}
	if !got[0].From.Equal(at(10)) || !got[0].To.Equal(at(30)) || !got[0].FetchedAt.Equal(at(200)) {
		t.Errorf("merged[0] = %+v", got[0])
	}
	if !got[1].From.Equal(at(40)) || !got[1].To.Equal(at(50)) {
		t.Errorf("merged[1] = %+v", got[1])
	}
}

func TestPlan_EmptyCacheFetchesWholeRange(t *testing.T) {
	now := at(1000)
	got := Plan(nil, at(0), now, now, time.Hour, 100*time.Second)
	if len(got) != 1 || !got[0].From.Equal(at(0)) || !got[0].To.Equal(now) {
		t.Fatalf("got %+v, want one range [0,1000]", got)
	}
}

func TestPlan_FullyCoveredFresh(t *testing.T) {
	now := at(1000)
	// covered [0,1000], fetched recently (age 10s < ttl 1h) -> live part fresh
	iv := []Interval{{From: at(0), To: now, FetchedAt: at(990)}}
	if got := Plan(iv, at(0), now, now, time.Hour, 100*time.Second); len(got) != 0 {
		t.Fatalf("expected no fetch, got %+v", got)
	}
}

func TestPlan_StaleLiveTailOnly(t *testing.T) {
	now := at(10000)
	ttl := 100 * time.Second
	live := 200 * time.Second // live zone = [now-200, now]
	// covered [0, now] but fetched long ago (age 5000s > ttl) -> only live tail refetched
	iv := []Interval{{From: at(0), To: now, FetchedAt: at(5000)}}
	got := Plan(iv, at(0), now, now, ttl, live)
	if len(got) != 1 || !got[0].From.Equal(now.Add(-live)) || !got[0].To.Equal(now) {
		t.Fatalf("got %+v, want only stale live tail [now-live, now]", got)
	}
}

func TestPlan_HistoricalImmutableNotRefetched(t *testing.T) {
	now := at(10000)
	live := 200 * time.Second
	// old interval covering only history [0, now-1000], fetched ages ago.
	iv := []Interval{{From: at(0), To: now.Add(-1000 * time.Second), FetchedAt: at(1)}}
	got := Plan(iv, at(0), now, now, 100*time.Second, live)
	// history [0, now-1000] stays covered; gap is (now-1000, now]
	if len(got) != 1 || !got[0].From.Equal(now.Add(-1000*time.Second)) || !got[0].To.Equal(now) {
		t.Fatalf("got %+v, want single gap (now-1000, now]", got)
	}
}

func TestPlan_BackfillGap(t *testing.T) {
	now := at(10000)
	iv := []Interval{{From: at(5000), To: now, FetchedAt: at(9990)}} // fresh, covers [5000, now]
	got := Plan(iv, at(0), now, now, time.Hour, 100*time.Second)
	if len(got) != 1 || !got[0].From.Equal(at(0)) || !got[0].To.Equal(at(5000)) {
		t.Fatalf("got %+v, want backfill gap [0,5000]", got)
	}
}
