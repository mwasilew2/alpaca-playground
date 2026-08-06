package store

import (
	"sort"
	"time"
)

// Interval is a fetched wall-clock range for one (symbol,timeframe). From/To are
// the requested FETCH bounds (not bar timestamps), so a fetched-but-empty range
// (market closed) is distinguishable from a never-fetched range. Inclusive.
type Interval struct {
	From, To, FetchedAt time.Time
}

// Range is a window that must be fetched, produced by Plan.
type Range struct {
	From, To time.Time
}

// sliverThreshold: gaps separated by less than this are merged into one fetch to
// avoid emitting many micro-requests around tiny already-covered slices.
const sliverThreshold = time.Minute

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Coalesce merges overlapping/adjacent intervals (sorted by From), taking
// FetchedAt = max within each merged group. Returns a new minimal, sorted set.
func Coalesce(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	s := append([]Interval(nil), in...)
	sort.Slice(s, func(i, j int) bool { return s[i].From.Before(s[j].From) })
	out := []Interval{s[0]}
	for _, iv := range s[1:] {
		last := &out[len(out)-1]
		if !iv.From.After(last.To) { // overlaps or touches
			if iv.To.After(last.To) {
				last.To = iv.To
			}
			if iv.FetchedAt.After(last.FetchedAt) {
				last.FetchedAt = iv.FetchedAt
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// Plan returns the sub-ranges of [start,end] that must be fetched: never-covered
// gaps plus a stale live tail. intervals should already be Coalesced. ttl is the
// freshness age for the live zone; liveHorizon is how far back from now bars are
// still mutable. now is injected.
func Plan(intervals []Interval, start, end, now time.Time, ttl, liveHorizon time.Duration) []Range {
	if !start.Before(end) {
		return nil
	}
	liveStart := now.Add(-liveHorizon) // t >= liveStart is "live"
	freshAfter := now.Add(-ttl)        // FetchedAt >= freshAfter is "fresh"

	var covered []Interval
	for _, iv := range intervals {
		f := maxTime(iv.From, start)
		t := minTime(iv.To, end)
		if !f.Before(t) {
			continue // no overlap with [start,end]
		}
		// historical portion [f, min(t, liveStart)] is always covered
		if hEnd := minTime(t, liveStart); f.Before(hEnd) {
			covered = append(covered, Interval{From: f, To: hEnd})
		}
		// live portion [max(f, liveStart), t] is covered only if the fetch is fresh
		if lStart := maxTime(f, liveStart); lStart.Before(t) && !iv.FetchedAt.Before(freshAfter) {
			covered = append(covered, Interval{From: lStart, To: t})
		}
	}
	covered = Coalesce(covered)
	return mergeSlivers(subtract(start, end, covered), sliverThreshold)
}

// subtract returns [start,end] minus the covered set (sorted, coalesced, each
// already clipped within [start,end]).
func subtract(start, end time.Time, covered []Interval) []Range {
	var gaps []Range
	cur := start
	for _, c := range covered {
		if c.From.After(cur) {
			gaps = append(gaps, Range{From: cur, To: c.From})
		}
		if c.To.After(cur) {
			cur = c.To
		}
	}
	if cur.Before(end) {
		gaps = append(gaps, Range{From: cur, To: end})
	}
	return gaps
}

// mergeSlivers merges gaps separated by <= threshold into one range.
func mergeSlivers(gaps []Range, threshold time.Duration) []Range {
	if len(gaps) == 0 {
		return nil
	}
	out := []Range{gaps[0]}
	for _, g := range gaps[1:] {
		last := &out[len(out)-1]
		if g.From.Sub(last.To) <= threshold {
			last.To = g.To
			continue
		}
		out = append(out, g)
	}
	return out
}
