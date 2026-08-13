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
	got := coalesce(in)
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

func TestNormalizeCoverage_PreservesLiveFreshness(t *testing.T) {
	// X and Y touch at t1000 but were fetched at different times. Coalesce would
	// merge them and stamp X's range with Y's newer FetchedAt (the bug).
	// normalizeCoverage keeps them distinct while both are live.
	x := Interval{From: at(700), To: at(1000), FetchedAt: at(1000)}
	y := Interval{From: at(1000), To: at(1140), FetchedAt: at(1140)}
	got := normalizeCoverage([]Interval{x, y}, at(500)) // both live (To > 500)
	if len(got) != 2 {
		t.Fatalf("want 2 distinct intervals, got %d: %+v", len(got), got)
	}
	if !got[0].From.Equal(at(700)) || !got[0].To.Equal(at(1000)) || !got[0].FetchedAt.Equal(at(1000)) {
		t.Errorf("segment[0] = %+v, want [700,1000]@1000", got[0])
	}
	if !got[1].From.Equal(at(1000)) || !got[1].To.Equal(at(1140)) || !got[1].FetchedAt.Equal(at(1140)) {
		t.Errorf("segment[1] = %+v, want [1000,1140]@1140", got[1])
	}
}

func TestNormalizeCoverage_CompactsHistory(t *testing.T) {
	// Same touching intervals, but now both are historical (To <= historyBefore):
	// freshness is irrelevant, so they collapse to a single interval (bounds growth).
	x := Interval{From: at(700), To: at(1000), FetchedAt: at(1000)}
	y := Interval{From: at(1000), To: at(1140), FetchedAt: at(1140)}
	got := normalizeCoverage([]Interval{x, y}, at(2000)) // both fully historical
	if len(got) != 1 || !got[0].From.Equal(at(700)) || !got[0].To.Equal(at(1140)) {
		t.Fatalf("want single merged historical interval [700,1140], got %+v", got)
	}
}

func TestNormalizeCoverage_OverlapNewestWins(t *testing.T) {
	// Overlapping fetches: the overlap [1000,1100] takes the newer FetchedAt and
	// merges with [1100,1200]; the older interval's exclusive left part [700,1000]
	// keeps its own older FetchedAt.
	x := Interval{From: at(700), To: at(1100), FetchedAt: at(1000)}
	y := Interval{From: at(1000), To: at(1200), FetchedAt: at(1200)}
	got := normalizeCoverage([]Interval{x, y}, at(0)) // all live
	if len(got) != 2 {
		t.Fatalf("want 2 intervals, got %d: %+v", len(got), got)
	}
	if !got[0].From.Equal(at(700)) || !got[0].To.Equal(at(1000)) || !got[0].FetchedAt.Equal(at(1000)) {
		t.Errorf("segment[0] = %+v, want [700,1000]@1000", got[0])
	}
	if !got[1].From.Equal(at(1000)) || !got[1].To.Equal(at(1200)) || !got[1].FetchedAt.Equal(at(1200)) {
		t.Errorf("segment[1] = %+v, want [1000,1200]@1200", got[1])
	}
}

func TestPlan_CombinedHistoricalGapAndStaleLiveTail(t *testing.T) {
	now := at(10000)
	ttl := 100 * time.Second
	live := 200 * time.Second // liveStart = 9800, freshAfter = 9900
	// A fresh historical island [4000,5000], plus an old fetch [9000,now] whose
	// live tail is stale. One Plan call must yield BOTH historical gaps AND the
	// stale live tail.
	iv := []Interval{
		{From: at(4000), To: at(5000), FetchedAt: at(9990)},
		{From: at(9000), To: now, FetchedAt: at(1000)},
	}
	got := Plan(iv, at(0), now, now, ttl, live)
	want := []Range{
		{From: at(0), To: at(4000)},    // historical backfill
		{From: at(5000), To: at(9000)}, // historical gap between islands
		{From: at(9800), To: now},      // stale live tail [now-live, now]
	}
	if len(got) != len(want) {
		t.Fatalf("got %d ranges, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].From.Equal(want[i].From) || !got[i].To.Equal(want[i].To) {
			t.Errorf("range[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPlan_MergesSubThresholdGaps(t *testing.T) {
	now := at(10000)
	// A tiny fresh covered island (30s wide, < sliverThreshold=1m) splits [0,now]
	// into two gaps only 30s apart; mergeSlivers coalesces them into one fetch range.
	iv := []Interval{{From: at(5000), To: at(5030), FetchedAt: at(9990)}}
	got := Plan(iv, at(0), now, now, time.Hour, 100*time.Second)
	if len(got) != 1 || !got[0].From.Equal(at(0)) || !got[0].To.Equal(now) {
		t.Fatalf("got %+v, want single merged range [0,now]", got)
	}
}

func TestPlan_ForgivesTrailingGapWithinTTL(t *testing.T) {
	now := at(10000)
	// Callers pass end=now(), so a repeat request always finds its final
	// instants uncovered. Chasing that sliver makes the ttl unreachable: every
	// request is a miss, and each leaves an interval that cannot merge.
	iv := []Interval{{From: at(0), To: at(9990), FetchedAt: at(9990)}}
	if got := Plan(iv, at(0), now, now, 100*time.Second, 200*time.Second); len(got) != 0 {
		t.Fatalf("expected the 10s trailing gap to be forgiven, got %+v", got)
	}
}

func TestPlan_StillFetchesTrailingGapWiderThanTTL(t *testing.T) {
	now := at(10000)
	// Fetched 10s ago (fresh) but only reached at(9800): a 200s tail, wider
	// than the 100s ttl, so the ttl cannot excuse it.
	iv := []Interval{{From: at(0), To: at(9800), FetchedAt: at(9990)}}
	got := Plan(iv, at(0), now, now, 100*time.Second, 400*time.Second)
	if len(got) != 1 || !got[0].From.Equal(at(9800)) || !got[0].To.Equal(now) {
		t.Fatalf("got %+v, want the trailing gap [9800, now]", got)
	}
}

func TestPlan_NeverForgivesAColdCache(t *testing.T) {
	now := at(10000)
	// The whole window is inside the last ttl, but nothing precedes the gap:
	// there is no recent fetch to excuse it.
	got := Plan(nil, at(9950), now, now, 100*time.Second, 200*time.Second)
	if len(got) != 1 || !got[0].From.Equal(at(9950)) || !got[0].To.Equal(now) {
		t.Fatalf("got %+v, want the whole window fetched", got)
	}
}

func TestPlan_ForgivesOnlyTheTrailingGap(t *testing.T) {
	now := at(10000) // freshAfter = 9900
	iv := []Interval{
		{From: at(0), To: at(9000), FetchedAt: at(9990)},
		{From: at(9950), To: at(9990), FetchedAt: at(9990)},
	}
	got := Plan(iv, at(0), now, now, 100*time.Second, 2000*time.Second)
	if len(got) != 1 || !got[0].From.Equal(at(9000)) || !got[0].To.Equal(at(9950)) {
		t.Fatalf("got %+v, want only the interior gap [9000,9950]", got)
	}
}
