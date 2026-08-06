package store

import (
	"sort"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
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

// normalizeCoverage collapses a set of fetched intervals into a minimal, sorted,
// non-overlapping set that PRESERVES per-region freshness. Unlike Coalesce (which
// takes FetchedAt = max across a whole merged span and would overstate the
// freshness of an older sub-range that a newer neighbour merely touches), each
// output segment carries the most-recent FetchedAt of the intervals that actually
// cover it. Adjacent live segments with different FetchedAt stay distinct — so a
// stale live sub-range is never masked by a fresher neighbour. Segments that have
// aged entirely into history (To <= historyBefore) merge freely regardless of
// FetchedAt, since Plan ignores freshness for immutable history; this bounds the
// set to roughly the live zone's width.
func normalizeCoverage(in []Interval, historyBefore time.Time) []Interval {
	// Distinct boundaries of all non-empty intervals partition the timeline into
	// elementary segments that lie wholly inside or wholly outside each interval.
	var bounds []time.Time
	for _, iv := range in {
		if iv.From.Before(iv.To) {
			bounds = append(bounds, iv.From, iv.To)
		}
	}
	if len(bounds) == 0 {
		return nil
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].Before(bounds[j]) })
	uniq := bounds[:1]
	for _, b := range bounds[1:] {
		if !b.Equal(uniq[len(uniq)-1]) {
			uniq = append(uniq, b)
		}
	}

	var out []Interval
	for i := 0; i+1 < len(uniq); i++ {
		lo, hi := uniq[i], uniq[i+1]
		var fa time.Time
		covered := false
		for _, iv := range in {
			if !iv.From.After(lo) && !iv.To.Before(hi) { // iv.From <= lo && iv.To >= hi
				covered = true
				fa = maxTime(fa, iv.FetchedAt)
			}
		}
		if !covered {
			continue // a genuine gap between fetched intervals
		}
		if n := len(out); n > 0 && out[n-1].To.Equal(lo) {
			last := &out[n-1]
			bothHistorical := !last.To.After(historyBefore) && !hi.After(historyBefore)
			if bothHistorical || last.FetchedAt.Equal(fa) {
				last.To = hi
				last.FetchedAt = maxTime(last.FetchedAt, fa)
				continue
			}
		}
		out = append(out, Interval{From: lo, To: hi, FetchedAt: fa})
	}
	return out
}

// Plan returns the sub-ranges of [start,end] that must be fetched: never-covered
// gaps plus a stale live tail. intervals must be non-overlapping with per-region
// FetchedAt (see normalizeCoverage); Plan reads each interval's own FetchedAt, so
// it must NOT be pre-Coalesced (that would overstate live freshness). ttl is the
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

// mergeBars unions two ascending-or-unsorted bar slices, deduping by T (b wins on
// a tie, being the fresher fetch), returning a slice sorted ascending by T.
func mergeBars(a, b []marketdata.Bar) []marketdata.Bar {
	idx := make(map[int64]marketdata.Bar, len(a)+len(b))
	order := make([]int64, 0, len(a)+len(b))
	add := func(bars []marketdata.Bar) {
		for _, x := range bars {
			ns := x.T.UnixNano()
			if _, ok := idx[ns]; !ok {
				order = append(order, ns)
			}
			idx[ns] = x
		}
	}
	add(a)
	add(b)
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]marketdata.Bar, 0, len(order))
	for _, ns := range order {
		out = append(out, idx[ns])
	}
	return out
}

// clip returns the bars with start <= T <= end.
func clip(bars []marketdata.Bar, start, end time.Time) []marketdata.Bar {
	var out []marketdata.Bar
	for _, b := range bars {
		if b.T.Before(start) || b.T.After(end) {
			continue
		}
		out = append(out, b)
	}
	return out
}
