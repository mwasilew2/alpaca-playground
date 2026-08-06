# Persistent Bar Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers-extended-cc:subagent-driven-development (recommended) or superpowers-extended-cc:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `internal/store` into a swappable **bar repository** — bars stored as rows (slicing pushed into the engine), full interval-based coverage tracking with incremental fetch, a durable **SQLite** backend selectable via env var, and `memory` as the default.

**Architecture:** Hexagonal. The `Store` core owns all caching policy (pure interval math: coverage, freshness, gap planning, coalescing); a narrow `Repository` port does dumb storage of bars + fetched intervals; `memRepo` and `sqlitestore` are adapters. `main` injects the chosen adapter. `Store.Get`'s signature is unchanged, so the poller and httpapi are untouched.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure-Go, cgo-free), `database/sql`, existing OTel/config.

**User decisions (already made):**
- Motivation: "fewer API calls over time" — durable local store, single-process, data fits in memory.
- Layering: swappable backend behind a narrow port; **policy in the core**, storage dumb (Approach A).
- Backend: **SQLite** via `modernc.org/sqlite`, bars as **rows**, slicing in SQL; `memory` is the default.
- Freshness: **immutable history + fresh tail**. Coverage: **full interval set** (fetch only true gaps + stale tail).
- Config: **env vars** `STORAGE=memory|disk`, `STORAGE_PATH`; no CLI flags.

Spec: `docs/superpowers/specs/2026-07-14-persistent-bar-store-design.md`.

**Sequencing note:** Tasks 1–4 are **additive** — the existing in-memory `Store` keeps serving and `go build ./...` / `go test ./...` stay green throughout. Task 5 performs the swap (rewrites `Store` + wires `main`) in one shot so the build never breaks.

---

### Task 1: Coverage interval math (pure)

**Goal:** Add the pure `Interval`/`Range` types and the coverage algorithm (`Coalesce`, `Plan`) with exhaustive unit tests. No I/O, no clock — `now` is injected.

**Files:**
- Create: `internal/store/coverage.go`
- Create: `internal/store/coverage_test.go`

**Acceptance Criteria:**
- [ ] `Coalesce` merges overlapping/adjacent intervals, sorted by `From`, taking `FetchedAt = max`.
- [ ] `Plan` returns `∅` when `[start,end]` is fully covered (historical by any interval; live only by a fresh one).
- [ ] `Plan` returns only never-covered gaps plus a stale live tail; a stale live interval yields `[now−liveHorizon, end]`, historical stays untouched.
- [ ] Gaps closer than the sliver threshold are merged into one range.

**Verify:** `go test ./internal/store/ -run 'Coalesce|Plan' -race -v` → PASS

**Steps:**

- [ ] **Step 1: Write the failing test** (`internal/store/coverage_test.go`)

```go
package store

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return base.Add(d) }

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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/ -run 'Coalesce|Plan' -v`
Expected: FAIL (undefined `Interval`, `Coalesce`, `Plan`, `Range`).

- [ ] **Step 3: Implement** (`internal/store/coverage.go`)

```go
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
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/store/ -run 'Coalesce|Plan' -race -v`
Expected: PASS (5 tests). Then `go build ./... && go vet ./... && gofmt -l .` clean, and `go test ./... -race` still green (old Store untouched).

- [ ] **Step 5: Commit**

```bash
git add internal/store/coverage.go internal/store/coverage_test.go
git commit --no-gpg-sign -m "feat(store): add pure interval coverage math (Coalesce, Plan)"
```

```json:metadata
{"files": ["internal/store/coverage.go", "internal/store/coverage_test.go"], "verifyCommand": "go test ./internal/store/ -run 'Coalesce|Plan' -race -v", "acceptanceCriteria": ["Coalesce merges overlapping/adjacent taking max FetchedAt", "Plan returns empty when fully covered (historical any / live fresh)", "Plan returns gaps + stale live tail only", "sub-threshold gaps merged"], "modelTier": "standard"}
```

---

### Task 2: Repository port + memRepo + contract suite

**Goal:** Define the `Repository` port, implement the in-memory adapter (`memRepo`), a shared adapter-conformance suite, and add `ranges.LiveHorizonForTimeframe`. Additive — old `Store` still compiles.

**Files:**
- Create: `internal/store/repository.go`
- Create: `internal/store/memory.go`
- Create: `internal/store/memory_test.go`
- Create: `internal/store/storetest/contract.go`
- Modify: `internal/ranges/ranges.go` (add `LiveHorizonForTimeframe`)
- Create: `internal/ranges/ranges_livehorizon_test.go`

**Acceptance Criteria:**
- [ ] `Repository` interface: `Bars`, `Intervals`, `PutBars`, `PutIntervals`, `Close`.
- [ ] `memRepo.Bars` returns only bars with `start ≤ T ≤ end`, ascending; `PutBars` upserts by `T`.
- [ ] `PutIntervals` replaces the key's interval set; `Intervals` round-trips it.
- [ ] `RunRepositoryContract` passes against `memRepo`.
- [ ] `ranges.LiveHorizonForTimeframe` returns the spec values; live horizon ≥ its TTL for each timeframe.

**Verify:** `go test ./internal/store/... ./internal/ranges/... -race` → PASS

**Steps:**

- [ ] **Step 1: `internal/store/repository.go`**

```go
package store

import (
	"context"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

// Repository is dumb persistent storage of bars + fetched intervals for
// (symbol,timeframe). It holds NO caching policy; memory and SQLite implement it
// identically. The core (Store) supplies already-coalesced intervals.
type Repository interface {
	// Bars returns cached bars for the key within [start,end], ascending by T.
	Bars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)
	// Intervals returns the stored fetched ranges for the key.
	Intervals(ctx context.Context, symbol, timeframe string) ([]Interval, error)
	// PutBars upserts bars keyed by (symbol,timeframe,T); corrected live bars overwrite.
	PutBars(ctx context.Context, symbol, timeframe string, bars []marketdata.Bar) error
	// PutIntervals replaces the key's stored interval set with the given set.
	PutIntervals(ctx context.Context, symbol, timeframe string, intervals []Interval) error
	// Close releases resources (no-op for memory).
	Close() error
}
```

- [ ] **Step 2: `internal/store/memory.go`**

```go
package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

// memRepo is an in-memory Repository. Bars are kept sorted by T per key.
type memRepo struct {
	mu        sync.Mutex
	bars      map[string][]marketdata.Bar
	intervals map[string][]Interval
}

// NewMemRepository returns an in-memory Repository (the default backend).
func NewMemRepository() Repository {
	return &memRepo{
		bars:      make(map[string][]marketdata.Bar),
		intervals: make(map[string][]Interval),
	}
}

func (m *memRepo) Bars(_ context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.bars[key(symbol, timeframe)]
	var out []marketdata.Bar
	for _, b := range all {
		if b.T.Before(start) {
			continue
		}
		if b.T.After(end) {
			break
		}
		out = append(out, b)
	}
	return out, nil
}

func (m *memRepo) Intervals(_ context.Context, symbol, timeframe string) ([]Interval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Interval(nil), m.intervals[key(symbol, timeframe)]...), nil
}

func (m *memRepo) PutBars(_ context.Context, symbol, timeframe string, bars []marketdata.Bar) error {
	if len(bars) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(symbol, timeframe)
	idx := make(map[int64]int, len(m.bars[k]))
	merged := append([]marketdata.Bar(nil), m.bars[k]...)
	for i, b := range merged {
		idx[b.T.UnixNano()] = i
	}
	for _, b := range bars {
		if i, ok := idx[b.T.UnixNano()]; ok {
			merged[i] = b // upsert
			continue
		}
		idx[b.T.UnixNano()] = len(merged)
		merged = append(merged, b)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].T.Before(merged[j].T) })
	m.bars[k] = merged
	return nil
}

func (m *memRepo) PutIntervals(_ context.Context, symbol, timeframe string, intervals []Interval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.intervals[key(symbol, timeframe)] = append([]Interval(nil), intervals...)
	return nil
}

func (m *memRepo) Close() error { return nil }
```

> Note: `key(symbol, timeframe)` already exists in the current `internal/store/store.go` (same package). Keep it there; Task 5 preserves it.

- [ ] **Step 3: `internal/store/storetest/contract.go`** (shared conformance suite both adapters run)

```go
// Package storetest provides a conformance suite that every store.Repository
// implementation must pass, guaranteeing memory and SQLite behave identically.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/store"
)

// RunRepositoryContract exercises a Repository. newRepo must return a fresh,
// empty Repository each call.
func RunRepositoryContract(t *testing.T, newRepo func(t *testing.T) store.Repository) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tb := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

	t.Run("bars round-trip and range slice", func(t *testing.T) {
		r := newRepo(t)
		defer r.Close()
		bars := []marketdata.Bar{
			{T: tb(0), C: 1}, {T: tb(1), C: 2}, {T: tb(2), C: 3},
		}
		if err := r.PutBars(ctx, "AAPL", "1Min", bars); err != nil {
			t.Fatal(err)
		}
		got, err := r.Bars(ctx, "AAPL", "1Min", tb(1), tb(2))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || !got[0].T.Equal(tb(1)) || got[1].C != 3 {
			t.Fatalf("range slice wrong: %+v", got)
		}
	})

	t.Run("PutBars upserts by T", func(t *testing.T) {
		r := newRepo(t)
		defer r.Close()
		_ = r.PutBars(ctx, "AAPL", "1Min", []marketdata.Bar{{T: tb(0), C: 1}})
		_ = r.PutBars(ctx, "AAPL", "1Min", []marketdata.Bar{{T: tb(0), C: 9}}) // overwrite
		got, _ := r.Bars(ctx, "AAPL", "1Min", tb(0), tb(0))
		if len(got) != 1 || got[0].C != 9 {
			t.Fatalf("upsert failed: %+v", got)
		}
	})

	t.Run("intervals replace + round-trip", func(t *testing.T) {
		r := newRepo(t)
		defer r.Close()
		in := []store.Interval{{From: tb(0), To: tb(5), FetchedAt: tb(5)}}
		if err := r.PutIntervals(ctx, "AAPL", "1Day", in); err != nil {
			t.Fatal(err)
		}
		got, _ := r.Intervals(ctx, "AAPL", "1Day")
		if len(got) != 1 || !got[0].From.Equal(tb(0)) || !got[0].To.Equal(tb(5)) {
			t.Fatalf("intervals round-trip wrong: %+v", got)
		}
		// replace with a different set
		_ = r.PutIntervals(ctx, "AAPL", "1Day", []store.Interval{{From: tb(0), To: tb(9), FetchedAt: tb(9)}})
		got, _ = r.Intervals(ctx, "AAPL", "1Day")
		if len(got) != 1 || !got[0].To.Equal(tb(9)) {
			t.Fatalf("intervals replace wrong: %+v", got)
		}
	})

	t.Run("empty keys return empty, not error", func(t *testing.T) {
		r := newRepo(t)
		defer r.Close()
		b, err := r.Bars(ctx, "NONE", "1Min", tb(0), tb(9))
		if err != nil || len(b) != 0 {
			t.Fatalf("empty bars: %+v err=%v", b, err)
		}
		iv, err := r.Intervals(ctx, "NONE", "1Min")
		if err != nil || len(iv) != 0 {
			t.Fatalf("empty intervals: %+v err=%v", iv, err)
		}
	})
}
```

- [ ] **Step 4: `internal/store/memory_test.go`**

```go
package store_test

import (
	"testing"

	"github.com/mwasilew2/alpaca-playground/internal/store"
	"github.com/mwasilew2/alpaca-playground/internal/store/storetest"
)

func TestMemRepository_Contract(t *testing.T) {
	storetest.RunRepositoryContract(t, func(t *testing.T) store.Repository {
		return store.NewMemRepository()
	})
}
```

- [ ] **Step 5: `internal/ranges/ranges.go` — add `LiveHorizonForTimeframe`**

Append this function (next to `TTLForTimeframe`):

```go
// LiveHorizonForTimeframe is how far back from now bars are still mutable
// (current/last-few periods + late trades). Older bars are treated as immutable.
func LiveHorizonForTimeframe(tf string) time.Duration {
	switch tf {
	case "1Min":
		return 15 * time.Minute
	case "5Min":
		return time.Hour
	case "1Hour":
		return 6 * time.Hour
	case "1Day":
		return 48 * time.Hour
	case "1Week":
		return 14 * 24 * time.Hour
	case "1Month":
		return 60 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}
```

- [ ] **Step 6: `internal/ranges/ranges_livehorizon_test.go`**

```go
package ranges

import "testing"

func TestLiveHorizonForTimeframe(t *testing.T) {
	for _, tf := range []string{"1Min", "5Min", "1Hour", "1Day", "1Week", "1Month"} {
		if lh := LiveHorizonForTimeframe(tf); lh < TTLForTimeframe(tf) {
			t.Errorf("liveHorizon(%s)=%v should be >= ttl %v", tf, lh, TTLForTimeframe(tf))
		}
	}
	if LiveHorizonForTimeframe("bogus") != 24*60*60*1e9 {
		t.Error("default live horizon should be 24h")
	}
}
```

- [ ] **Step 7: Verify + commit**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./internal/store/... ./internal/ranges/... -race`
Expected: build/vet/fmt clean, tests PASS (contract + livehorizon). Whole suite `go test ./... -race` still green.

```bash
git add internal/store/repository.go internal/store/memory.go internal/store/memory_test.go internal/store/storetest internal/ranges/ranges.go internal/ranges/ranges_livehorizon_test.go
git commit --no-gpg-sign -m "feat(store): add Repository port, memRepo, contract suite, LiveHorizonForTimeframe"
```

```json:metadata
{"files": ["internal/store/repository.go", "internal/store/memory.go", "internal/store/memory_test.go", "internal/store/storetest/contract.go", "internal/ranges/ranges.go", "internal/ranges/ranges_livehorizon_test.go"], "verifyCommand": "go test ./internal/store/... ./internal/ranges/... -race", "acceptanceCriteria": ["Repository interface defined", "memRepo range-slices + upserts by T", "PutIntervals replaces set; Intervals round-trips", "RunRepositoryContract passes for memRepo", "LiveHorizonForTimeframe returns spec values, >= ttl"], "modelTier": "standard"}
```

---

### Task 3: SQLite adapter (`sqlitestore`)

**Goal:** Implement `Repository` backed by SQLite (`modernc.org/sqlite`, cgo-free) and run the shared contract suite against it.

**Files:**
- Create: `internal/store/sqlitestore/sqlitestore.go`
- Create: `internal/store/sqlitestore/sqlitestore_test.go`
- Modify: `go.mod`, `go.sum` (via `go get`)

**Acceptance Criteria:**
- [ ] `Open(path)` creates the parent dir + DB, sets WAL/busy_timeout/synchronous PRAGMAs, creates schema (`schema_meta.version=1`).
- [ ] Implements `store.Repository`: `Bars` (SQL `t BETWEEN` slice), `PutBars` (upsert), `Intervals`, `PutIntervals` (replace in a txn), `Close`.
- [ ] `RunRepositoryContract` passes against a `t.TempDir()` SQLite DB.

**Verify:** `go test ./internal/store/sqlitestore/ -race -v` → PASS

**Steps:**

- [ ] **Step 1: Add the dependency**

```bash
go get modernc.org/sqlite@latest
```

- [ ] **Step 2: `internal/store/sqlitestore/sqlitestore.go`**

```go
// Package sqlitestore is a durable store.Repository backed by SQLite
// (modernc.org/sqlite — pure Go, cgo-free). Bars are rows so the engine slices.
package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/store"

	_ "modernc.org/sqlite"
)

type repo struct{ db *sql.DB }

// Open opens (creating if needed) a SQLite-backed Repository at path.
func Open(path string) (store.Repository, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlitestore: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // modernc + single-writer: serialize to avoid lock churn
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlitestore: %s: %w", pragma, err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &repo{db: db}, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS bars (
			symbol TEXT NOT NULL, timeframe TEXT NOT NULL, t INTEGER NOT NULL,
			o REAL, h REAL, l REAL, c REAL, v INTEGER, n INTEGER, vw REAL,
			PRIMARY KEY (symbol, timeframe, t)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS fetched_ranges (
			symbol TEXT NOT NULL, timeframe TEXT NOT NULL,
			from_ns INTEGER NOT NULL, to_ns INTEGER NOT NULL, fetched_at_ns INTEGER NOT NULL,
			PRIMARY KEY (symbol, timeframe, from_ns)
		)`,
		`CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`,
		`INSERT INTO schema_meta (version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_meta)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("sqlitestore: migrate: %w", err)
		}
	}
	return nil
}

func (r *repo) Bars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT t,o,h,l,c,v,n,vw FROM bars
		 WHERE symbol=? AND timeframe=? AND t BETWEEN ? AND ? ORDER BY t`,
		symbol, timeframe, start.UnixNano(), end.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: bars query: %w", err)
	}
	defer rows.Close()
	var out []marketdata.Bar
	for rows.Next() {
		var tn int64
		var b marketdata.Bar
		if err := rows.Scan(&tn, &b.O, &b.H, &b.L, &b.C, &b.V, &b.N, &b.VW); err != nil {
			return nil, fmt.Errorf("sqlitestore: bars scan: %w", err)
		}
		b.T = time.Unix(0, tn).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *repo) Intervals(ctx context.Context, symbol, timeframe string) ([]store.Interval, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT from_ns,to_ns,fetched_at_ns FROM fetched_ranges WHERE symbol=? AND timeframe=?`,
		symbol, timeframe)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: intervals query: %w", err)
	}
	defer rows.Close()
	var out []store.Interval
	for rows.Next() {
		var f, t, fa int64
		if err := rows.Scan(&f, &t, &fa); err != nil {
			return nil, fmt.Errorf("sqlitestore: intervals scan: %w", err)
		}
		out = append(out, store.Interval{
			From: time.Unix(0, f).UTC(), To: time.Unix(0, t).UTC(), FetchedAt: time.Unix(0, fa).UTC(),
		})
	}
	return out, rows.Err()
}

func (r *repo) PutBars(ctx context.Context, symbol, timeframe string, bars []marketdata.Bar) error {
	if len(bars) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: putbars begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO bars (symbol,timeframe,t,o,h,l,c,v,n,vw) VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(symbol,timeframe,t) DO UPDATE SET
		   o=excluded.o,h=excluded.h,l=excluded.l,c=excluded.c,v=excluded.v,n=excluded.n,vw=excluded.vw`)
	if err != nil {
		return fmt.Errorf("sqlitestore: putbars prepare: %w", err)
	}
	defer stmt.Close()
	for _, b := range bars {
		if _, err := stmt.ExecContext(ctx, symbol, timeframe, b.T.UnixNano(), b.O, b.H, b.L, b.C, b.V, b.N, b.VW); err != nil {
			return fmt.Errorf("sqlitestore: putbars exec: %w", err)
		}
	}
	return tx.Commit()
}

func (r *repo) PutIntervals(ctx context.Context, symbol, timeframe string, intervals []store.Interval) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: putintervals begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM fetched_ranges WHERE symbol=? AND timeframe=?`, symbol, timeframe); err != nil {
		return fmt.Errorf("sqlitestore: putintervals delete: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO fetched_ranges (symbol,timeframe,from_ns,to_ns,fetched_at_ns) VALUES (?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("sqlitestore: putintervals prepare: %w", err)
	}
	defer stmt.Close()
	for _, iv := range intervals {
		if _, err := stmt.ExecContext(ctx, symbol, timeframe, iv.From.UnixNano(), iv.To.UnixNano(), iv.FetchedAt.UnixNano()); err != nil {
			return fmt.Errorf("sqlitestore: putintervals exec: %w", err)
		}
	}
	return tx.Commit()
}

func (r *repo) Close() error { return r.db.Close() }
```

- [ ] **Step 3: `internal/store/sqlitestore/sqlitestore_test.go`**

```go
package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/mwasilew2/alpaca-playground/internal/store"
	"github.com/mwasilew2/alpaca-playground/internal/store/sqlitestore"
	"github.com/mwasilew2/alpaca-playground/internal/store/storetest"
)

func TestSQLiteRepository_Contract(t *testing.T) {
	storetest.RunRepositoryContract(t, func(t *testing.T) store.Repository {
		r, err := sqlitestore.Open(filepath.Join(t.TempDir(), "cache.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return r
	})
}
```

- [ ] **Step 4: Verify + commit**

Run: `go mod tidy && go build ./... && go vet ./... && gofmt -l . && go test ./internal/store/sqlitestore/ -race -v`
Expected: contract PASS against SQLite. Whole suite `go test ./... -race` green. `go mod tidy` leaves modernc.org/sqlite as a direct dep.

```bash
git add internal/store/sqlitestore go.mod go.sum
git commit --no-gpg-sign -m "feat(store): add SQLite Repository adapter (modernc, WAL, bars-as-rows)"
```

```json:metadata
{"files": ["internal/store/sqlitestore/sqlitestore.go", "internal/store/sqlitestore/sqlitestore_test.go", "go.mod", "go.sum"], "verifyCommand": "go test ./internal/store/sqlitestore/ -race -v", "acceptanceCriteria": ["Open creates dir+DB, sets PRAGMAs, migrates schema v1", "implements Repository (Bars SQL slice, PutBars upsert, PutIntervals replace-in-txn)", "RunRepositoryContract passes on a temp-dir DB"], "modelTier": "standard"}
```

---

### Task 4: Config — storage backend selection

**Goal:** Add `STORAGE` / `STORAGE_PATH` to config with validation and `.env.example` docs. Additive.

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `.env.example`

**Acceptance Criteria:**
- [ ] `Config` gains `StorageBackend` (default `memory`) and `StoragePath` (default `./data/cache.db`).
- [ ] `Load` errors when `STORAGE` is set to anything other than `memory` or `disk`.
- [ ] `.env.example` documents both vars.

**Verify:** `go test ./internal/config/... -race -v` → PASS

**Steps:**

- [ ] **Step 1: Add fields + validation in `internal/config/config.go`**

Add to the `Config` struct (after `ServiceName`):

```go
	StorageBackend string // "memory" (default) or "disk"
	StoragePath    string // SQLite file path when StorageBackend == "disk"
```

In `Load`, add to the `cfg := &Config{...}` literal:

```go
		StorageBackend: def(getenv("STORAGE"), "memory"),
		StoragePath:    def(getenv("STORAGE_PATH"), "./data/cache.db"),
```

And before `return cfg, nil` (after the base-URL warning block), add validation:

```go
	if cfg.StorageBackend != "memory" && cfg.StorageBackend != "disk" {
		return nil, fmt.Errorf("STORAGE must be 'memory' or 'disk', got %q", cfg.StorageBackend)
	}
```

- [ ] **Step 2: Tests in `internal/config/config_test.go`**

Append:

```go
func TestLoad_StorageDefaults(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageBackend != "memory" || cfg.StoragePath != "./data/cache.db" {
		t.Errorf("storage defaults wrong: %q %q", cfg.StorageBackend, cfg.StoragePath)
	}
}

func TestLoad_StorageValidation(t *testing.T) {
	_, err := Load(envFrom(map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s", "STORAGE": "s3"}))
	if err == nil {
		t.Fatal("expected error for invalid STORAGE")
	}
	cfg, err := Load(envFrom(map[string]string{"ALPACA_API_KEY": "k", "ALPACA_API_SECRET": "s", "STORAGE": "disk", "STORAGE_PATH": "/tmp/x.db"}))
	if err != nil || cfg.StorageBackend != "disk" || cfg.StoragePath != "/tmp/x.db" {
		t.Fatalf("disk config wrong: %+v err=%v", cfg, err)
	}
}
```

- [ ] **Step 3: `.env.example`**

Add near the other vars:

```
# Cache storage: 'memory' (default) or 'disk' (persistent SQLite).
STORAGE=memory
# SQLite file path when STORAGE=disk (put on a volume under Docker for persistence).
STORAGE_PATH=./data/cache.db
```

- [ ] **Step 4: Verify + commit**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./internal/config/... -race -v`
Expected: PASS. Whole suite green.

```bash
git add internal/config/config.go internal/config/config_test.go .env.example
git commit --no-gpg-sign -m "feat(config): add STORAGE/STORAGE_PATH backend selection"
```

```json:metadata
{"files": ["internal/config/config.go", "internal/config/config_test.go", ".env.example"], "verifyCommand": "go test ./internal/config/... -race -v", "acceptanceCriteria": ["Config gains StorageBackend (default memory) + StoragePath (default ./data/cache.db)", "Load rejects STORAGE other than memory|disk", ".env.example documents both"], "modelTier": "mechanical"}
```

---

### Task 5: Swap the Store to the repository+coverage policy and wire it up

**Goal:** Rewrite `Store` to own the coverage/incremental-fetch policy over a `Repository` (with a per-key lock), replace the old store tests, wire the chosen backend in `main.go`, and drop the now-redundant `ranges.Slice` in httpapi. `Get`'s signature is unchanged. Build stays green after this task.

**Files:**
- Rewrite: `internal/store/store.go`
- Rewrite: `internal/store/store_test.go`
- Rewrite: `internal/store/store_correlation_test.go`
- Modify: `main.go`
- Modify: `internal/httpapi/handlers.go`

**Acceptance Criteria:**
- [ ] `Store.New(fetch FetchFunc, repo Repository, ttl, liveHorizon func(string) time.Duration) *Store`; `Get` signature unchanged (`ctx, symbol, timeframe, start, end → []marketdata.Bar, error`).
- [ ] On a fully-covered request `Get` performs **no** fetch and returns `repo.Bars`; on a miss it fetches only the planned ranges, records each fetched interval (even when empty), coalesces, and persists.
- [ ] Concurrent same-key `Get`s do not double-fetch the same gap (per-key lock).
- [ ] Persist failure degrades: the request is still served (from repo ∪ freshly-fetched bars) and an error is recorded, not returned.
- [ ] `main.go` builds `memRepo` or `sqlitestore.Open(cfg.StoragePath)` per `cfg.StorageBackend` and `defer repo.Close()`; `disk` open failure is fatal at startup.
- [ ] httpapi no longer calls `ranges.Slice` after `Get`; `go build ./... && go test ./... -race` green.

**Verify:** `go build ./... && go vet ./... && go test ./... -race` → PASS

**Steps:**

- [ ] **Step 1: Rewrite `internal/store/store.go`**

```go
package store

import (
	"context"
	"sync"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/observability"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// FetchFunc loads bars for one symbol/timeframe over [start,end] from upstream.
type FetchFunc func(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)

// Store is a read-through bar cache. It owns coverage/freshness/incremental-fetch
// policy (pure interval math) and delegates storage to a Repository.
type Store struct {
	fetch       FetchFunc
	repo        Repository
	ttl         func(timeframe string) time.Duration
	liveHorizon func(timeframe string) time.Duration
	now         func() time.Time

	tracer trace.Tracer
	hits   metric.Int64Counter
	misses metric.Int64Counter

	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// New builds a Store over repo. ttl and liveHorizon map a timeframe to its
// freshness age and mutable-window (use ranges.TTLForTimeframe / LiveHorizonForTimeframe).
func New(fetch FetchFunc, repo Repository, ttl, liveHorizon func(string) time.Duration) *Store {
	m := otel.Meter("alpaca-playground/store")
	hits, _ := m.Int64Counter("store.cache.hits")
	misses, _ := m.Int64Counter("store.cache.misses")
	return &Store{
		fetch:       fetch,
		repo:        repo,
		ttl:         ttl,
		liveHorizon: liveHorizon,
		now:         time.Now,
		tracer:      otel.Tracer("alpaca-playground/store"),
		hits:        hits,
		misses:      misses,
		locks:       make(map[string]*keyLock),
	}
}

func key(symbol, timeframe string) string { return symbol + "|" + timeframe }

// lockKey serializes Gets for a single (symbol,timeframe) so concurrent callers
// cannot double-fetch the same gap. Returns an unlock func.
func (s *Store) lockKey(k string) func() {
	s.mu.Lock()
	kl := s.locks[k]
	if kl == nil {
		kl = &keyLock{}
		s.locks[k] = kl
	}
	kl.refs++
	s.mu.Unlock()

	kl.mu.Lock()
	return func() {
		kl.mu.Unlock()
		s.mu.Lock()
		kl.refs--
		if kl.refs == 0 {
			delete(s.locks, k)
		}
		s.mu.Unlock()
	}
}

// Get returns cached bars for the key within [start,end], fetching only the
// never-covered gaps plus a stale live tail.
func (s *Store) Get(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	ctx, span := s.tracer.Start(ctx, "store.Get", trace.WithAttributes(
		attribute.String("symbol", symbol),
		attribute.String("timeframe", timeframe),
	))
	defer span.End()

	k := key(symbol, timeframe)
	now := s.now()

	unlock := s.lockKey(k)
	defer unlock()

	stored, err := s.repo.Intervals(ctx, symbol, timeframe)
	if err != nil {
		observability.RecordError(ctx, observability.ComponentStore, observability.KindInternal, err)
		return nil, err
	}
	coalesced := Coalesce(stored)
	toFetch := Plan(coalesced, start, end, now, s.ttl(timeframe), s.liveHorizon(timeframe))

	if len(toFetch) == 0 {
		span.SetAttributes(attribute.Bool("cache.hit", true))
		if s.hits != nil {
			s.hits.Add(ctx, 1)
		}
		return s.repo.Bars(ctx, symbol, timeframe, start, end)
	}

	span.SetAttributes(attribute.Bool("cache.hit", false), attribute.Int("fetch.ranges", len(toFetch)))
	if s.misses != nil {
		s.misses.Add(ctx, 1)
	}

	var fetchedIntervals []Interval
	var freshBars []marketdata.Bar
	persistFailed := false
	for _, r := range toFetch {
		bars, err := s.fetch(ctx, symbol, timeframe, r.From, r.To)
		if err != nil {
			return nil, err // alpaca client already recorded the error
		}
		fetchedIntervals = append(fetchedIntervals, Interval{From: r.From, To: r.To, FetchedAt: now})
		freshBars = append(freshBars, bars...)
		if perr := s.repo.PutBars(ctx, symbol, timeframe, bars); perr != nil {
			observability.RecordError(ctx, observability.ComponentStore, observability.KindInternal, perr)
			persistFailed = true
		}
	}
	newIntervals := Coalesce(append(coalesced, fetchedIntervals...))
	if !persistFailed {
		if perr := s.repo.PutIntervals(ctx, symbol, timeframe, newIntervals); perr != nil {
			observability.RecordError(ctx, observability.ComponentStore, observability.KindInternal, perr)
			persistFailed = true
		}
	}

	if persistFailed {
		// Degrade: serve from what the repo already had plus freshly-fetched bars.
		repoBars, _ := s.repo.Bars(ctx, symbol, timeframe, start, end)
		return clip(mergeBars(repoBars, freshBars), start, end), nil
	}
	return s.repo.Bars(ctx, symbol, timeframe, start, end)
}
```

- [ ] **Step 2: Add `mergeBars` + `clip` to `internal/store/coverage.go`** (bar helpers used by the degrade path)

```go
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
```

Update `internal/store/coverage.go`'s imports to include `"github.com/mwasilew2/alpaca-playground/internal/marketdata"` (alongside `sort`, `time`).

- [ ] **Step 3: Rewrite `internal/store/store_test.go`** (policy tests with a fake repo + fake fetch)

```go
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

func fixedTTL(d time.Duration) func(string) time.Duration    { return func(string) time.Duration { return d } }
func fixedLive(d time.Duration) func(string) time.Duration   { return func(string) time.Duration { return d } }

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
```

- [ ] **Step 4: Rewrite `internal/store/store_correlation_test.go`** (span nesting still holds under the new Store)

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestGet_EmitsCorrelatedSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	fetch := func(_ context.Context, s, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		return nil, nil
	}
	st := New(fetch, NewMemRepository(), fixedTTL(time.Minute), fixedLive(time.Hour))

	parentCtx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	if _, err := st.Get(parentCtx, "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	parent.End()

	var got sdktrace.ReadOnlySpan
	for _, sp := range sr.Ended() {
		if sp.Name() == "store.Get" {
			got = sp
		}
	}
	if got == nil {
		t.Fatal("no store.Get span recorded")
	}
	if got.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("store.Get not in parent trace")
	}
	if got.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("store.Get parent span id mismatch")
	}
}
```

> The old `store_test.go` tests (`TestGet_CacheHitSkipsFetch`, `TestGet_StaleRefetches`, `TestGet_EarlierStartRefetches`, `TestGet_Concurrent`) are replaced by the ones above — delete them (this Step's file content is the full new file).

- [ ] **Step 5: Wire `main.go`** — build the repo from config, pass to `store.New`, close on shutdown.

Add imports: `"github.com/mwasilew2/alpaca-playground/internal/store/sqlitestore"` (store + ranges already imported).

Replace the store construction line:

```go
	st := store.New(store.FetchFunc(client.GetBars), ranges.TTLForTimeframe)
```

with:

```go
	var repo store.Repository
	switch cfg.StorageBackend {
	case "disk":
		repo, err = sqlitestore.Open(cfg.StoragePath)
		if err != nil {
			return err
		}
	default:
		repo = store.NewMemRepository()
	}
	defer repo.Close()

	st := store.New(store.FetchFunc(client.GetBars), repo, ranges.TTLForTimeframe, ranges.LiveHorizonForTimeframe)
```

(`err` is already declared earlier in `run()`; reuse it.)

- [ ] **Step 6: httpapi — drop the redundant slice** in `internal/httpapi/handlers.go`

The `handleBars` block currently is:

```go
	out := ranges.Slice(bars, start)
	if out == nil {
		out = []marketdata.Bar{}
	}
	writeJSON(w, http.StatusOK, barsResponse{
		Symbol: symbol, Range: rangeStr, Timeframe: spec.Timeframe, Bars: out,
	})
```

Replace with (the store already returns bars within `[start,end]`):

```go
	if bars == nil {
		bars = []marketdata.Bar{}
	}
	writeJSON(w, http.StatusOK, barsResponse{
		Symbol: symbol, Range: rangeStr, Timeframe: spec.Timeframe, Bars: bars,
	})
```

If `ranges` becomes unused in `handlers.go` after this, remove it from the imports (it is still used for `ranges.Range`/`ranges.Resolve`, so it stays).

- [ ] **Step 7: Verify + commit**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -race`
Expected: build/vet/fmt clean; ALL packages PASS (new store policy tests, repository contracts for memory+sqlite, config, httpapi, poller, etc.).

Then a runtime check that disk persistence works and cuts calls (stub Alpaca; second identical run over the same DB fetches nothing new):

```bash
go build -o /tmp/alp . && DB=$(mktemp -u /tmp/cache-XXXX.db)
# start a 200-returning stub, run once with STORAGE=disk, capture; run again; expect the poller's
# store.Get to serve from disk on the second process. (Manual/observed; see the smoke pattern in scripts/.)
```

```bash
git add internal/store/store.go internal/store/coverage.go internal/store/store_test.go internal/store/store_correlation_test.go main.go internal/httpapi/handlers.go
git commit --no-gpg-sign -m "feat(store): swap to Repository + interval-coverage policy; wire backend selection"
```

```json:metadata
{"files": ["internal/store/store.go", "internal/store/coverage.go", "internal/store/store_test.go", "internal/store/store_correlation_test.go", "main.go", "internal/httpapi/handlers.go"], "verifyCommand": "go build ./... && go vet ./... && go test ./... -race", "acceptanceCriteria": ["New(fetch,repo,ttl,liveHorizon); Get signature unchanged", "fully-covered Get does no fetch; miss fetches only planned ranges + records empty intervals", "concurrent same-key Get does not double-fetch", "persist failure degrades (serves repo ∪ fresh), does not return error", "main selects memRepo/sqlitestore per config, defer Close, disk-open failure fatal", "httpapi drops redundant ranges.Slice; whole suite green"], "modelTier": "standard"}
```

---

## Self-Review

**Spec coverage:** §3 architecture → Tasks 1–5; §4 Repository port → Task 2; §5 coverage/freshness → Task 1 (+ liveHorizon in Task 2); §6 adapters → memRepo (Task 2), sqlitestore (Task 3); §7 concurrency (keyed lock) → Task 5; §8 config → Task 4; §9 error handling (fail-fast disk open, read-error return, persist degrade) → Task 5; §10 observability (hits/misses, span attrs, RecordError) → Task 5; §11 blast radius (Get unchanged, httpapi Slice drop, main wiring, ranges add) → Tasks 2 & 5; §12 testing (coverage pure, contract on both adapters, policy fakes, sqlite temp-dir) → Tasks 1–3, 5. ✓ No gaps.

**Placeholder scan:** every code step contains complete code; the runtime check in Task 5 Step 7 is an optional observed check, and each task's automated `Verify` command is concrete. ✓

**Type consistency:** `Interval{From,To,FetchedAt}`, `Range{From,To}`, `Repository{Bars,Intervals,PutBars,PutIntervals,Close}`, `NewMemRepository()`, `sqlitestore.Open`, `Store.New(fetch,repo,ttl,liveHorizon)`, `key(symbol,timeframe)`, `Coalesce`/`Plan`/`mergeBars`/`clip`, `ranges.LiveHorizonForTimeframe`, `observability.ComponentStore`/`KindInternal` — all referenced consistently across tasks and match the current codebase (`marketdata.Bar` fields T/O/H/L/C/V/N/VW; `observability` component/kind constants exist). ✓

**Note:** `coverage.go` imports `marketdata` only after Task 5 adds `mergeBars`/`clip`; in Task 1 it imports just `sort`,`time`. The engineer adds the import in Task 5 Step 2 (called out explicitly).
