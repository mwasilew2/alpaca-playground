# Persistent Bar Store — Design

**Date:** 2026-07-14
**Status:** Approved (design phase)
**Scope:** Redesign `internal/store` from an in-memory whole-window cache into a
swappable **bar repository** with a durable **SQLite** backend, storing bars as
rows so the engine slices, with full interval-based coverage tracking and
incremental fetching. Storage location (`memory`|`disk`) is selected via env var.

## 1. Purpose & Goal

Persist the bar cache so repeated runs reuse fetched data long-term and issue
**fewer Alpaca API calls** over time. Today `store.Store` keeps one whole-window
entry per `(symbol, timeframe)` in a `map` behind a mutex; it is lost on restart
and re-fetches an entire window on any coverage miss.

The redesign makes bars first-class rows in a queryable store, tracks exactly
which time ranges have been fetched, and fetches only the true gaps plus a stale
recent tail — minimizing API usage while surviving restarts.

## 2. Locked Decisions (from brainstorming)

- **Motivation:** fewer API calls over time (durable local store); single-process;
  dataset fits in memory but is persisted for reuse.
- **Layering:** swappable backend behind a **narrow storage port**; the caching
  **policy lives in the core** (`Store`), storage is dumb. (Approach A; B "smart
  repository" and C "injected policy decorator" rejected.)
- **Backend:** SQLite via **`modernc.org/sqlite`** (pure Go, cgo-free), bars as
  **rows**, slicing pushed into SQL. `memory` remains the default backend.
- **Freshness:** **immutable history + fresh tail** — completed historical bars
  cached indefinitely; only a recent live zone is re-fetched.
- **Coverage:** **full interval set** per key — fetch only true gaps + stale tail.
- **Config surface:** **env vars** (`STORAGE`, `STORAGE_PATH`), no CLI flags.

Design principles honored: SOLID (SRP/DIP/OCP/ISP), hexagonal (core + driven
ports + adapters), Ousterhout deep-modules/narrow-interfaces, clean code.

## 3. Architecture

```
httpapi / poller
      │  Get(ctx, symbol, timeframe, start, end) -> []Bar     (signature UNCHANGED)
      ▼
┌────────────────────────── Store (core: policy) ───────────────────────────┐
│  1. intervals ← repo.Intervals(sym,tf)                                      │
│  2. toFetch  ← coverage.Plan(intervals, start, end, now, ttl, liveHorizon)  │  PURE interval math
│  3. for range in toFetch: bars ← fetch(...); repo.PutBars(bars)             │  keyed singleflight per (sym,tf)
│  4. repo.PutIntervals( coverage.Coalesce(intervals ∪ fetched) )             │
│  5. return repo.Bars(sym,tf,start,end)         // engine slices             │
└──────────────────────────────────┬─────────────────────────────────────────┘
                                    │ Repository port (driven)
                 ┌──────────────────┴───────────────────┐
                 ▼                                        ▼
          memRepo (maps+mutex, internal/store)     sqlitestore (modernc.org/sqlite, database/sql, WAL)
```

**Driven ports:** `Repository` (storage) and `FetchFunc` (Alpaca, existing). The
core depends only on these interfaces; `main` injects the concrete adapters.

## 4. The Repository Port

```go
// Interval is a fetched wall-clock range. From/To are the requested FETCH
// bounds (not bar timestamps), so a fetched-but-empty range (market closed) is
// distinguishable from a never-fetched range.
type Interval struct {
    From, To, FetchedAt time.Time
}

// Repository is dumb storage of bars + fetched intervals for (symbol,timeframe).
// It holds NO caching policy. Both memory and SQLite implement it identically.
type Repository interface {
    // Bars returns cached bars for the key within [start,end], ascending by T.
    Bars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)
    // Intervals returns the stored (coalesced) fetched ranges for the key.
    Intervals(ctx context.Context, symbol, timeframe string) ([]Interval, error)
    // PutBars upserts bars, keyed by (symbol,timeframe,T); corrected live bars overwrite.
    PutBars(ctx context.Context, symbol, timeframe string, bars []marketdata.Bar) error
    // PutIntervals replaces the key's stored interval set with the given (coalesced) set.
    PutIntervals(ctx context.Context, symbol, timeframe string, intervals []Interval) error
    // Close releases resources (no-op for memory).
    Close() error
}
```

The core provides already-coalesced intervals to `PutIntervals`; the repo never
coalesces. All interval algebra lives in `coverage.go` (pure).

## 5. Coverage & Freshness (`internal/store/coverage.go`, pure)

**Two per-timeframe knobs:**
- `ttl(tf)` — how long a fetch of the live zone stays fresh. Reuse
  `ranges.TTLForTimeframe`.
- `liveHorizon(tf)` — how far back from `now` bars are still mutable. New
  `ranges.LiveHorizonForTimeframe`: 1Min→15m, 5Min→1h, 1Hour→6h, 1Day→2d,
  1Week→2w, 1Month→2mo. Older than this is immutable.

**Adequate coverage of point `t` in `[start,end]`:**
- `t < now − liveHorizon` (historical): covered iff `t` is in **any** interval
  (`FetchedAt` ignored — immutable).
- `t ≥ now − liveHorizon` (live): covered iff `t` is in an interval with
  `FetchedAt ≥ now − ttl` (fresh).

**`Plan(intervals, start, end, now, ttl, liveHorizon) → []Range`:**
1. `covered = (intervals clipped to historical part) ∪ (fresh intervals clipped
   to live part)`.
2. `toFetch = [start,end] − covered`; coalesce and merge sub-threshold slivers
   (a small gap-merge constant, e.g. a few bar-periods) to avoid micro-fetches.

**`Coalesce([]Interval) → []Interval`:** merges overlapping/adjacent intervals,
taking `FetchedAt = max`. Safe: merging only spans that touch; live-zone
freshness is set by the newest interval that actually covers it; any freshness
"overstatement" lands only on historical (immutable) sub-ranges.

**Record fetch bounds even when empty:** after fetching `[gFrom,gTo]`, store
`Interval{gFrom, gTo, now}` even if zero bars returned — this prevents
re-fetching known-empty ranges.

**Worked example** (1Day, ttl=1h, liveHorizon=2d, key `AAPL|1Day`):
- Empty cache, request `[−1y, now]` → fetch `[−1y, now]`.
- +5 min, same request → historical covered (immutable), live covered by a
  5-min-old fetch (<1h) → `toFetch = ∅`, **0 calls**.
- +2 h → live stale → fetch only `[now−2d, now]`; deep history untouched.
- request `[−5y, now]` → fetch only `[−5y, −1y]` (new backfill gap).

## 6. Storage Adapters

**memRepo** (`internal/store/memory.go`): `map[key][]marketdata.Bar` kept sorted
by `T` + `map[key][]Interval`, guarded by a `sync.Mutex`. `Bars` = binary-search
slice; `PutBars` = merge/upsert by `T`; `Intervals`/`PutIntervals` =
get/replace; `Close` = no-op. Default backend (preserves today's in-memory
behavior).

**sqlitestore** (`internal/store/sqlitestore/sqlitestore.go`): `database/sql`
with the `modernc.org/sqlite` driver. `Open(path)` creates the parent dir, opens
the DB, sets PRAGMAs (`journal_mode=WAL`, `busy_timeout=5000`,
`synchronous=NORMAL`), and runs migrations. Schema:

```sql
CREATE TABLE IF NOT EXISTS bars (
  symbol TEXT NOT NULL, timeframe TEXT NOT NULL, t INTEGER NOT NULL, -- unix nanos
  o REAL, h REAL, l REAL, c REAL, v INTEGER, n INTEGER, vw REAL,
  PRIMARY KEY (symbol, timeframe, t)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS fetched_ranges (
  symbol TEXT NOT NULL, timeframe TEXT NOT NULL,
  from_ns INTEGER NOT NULL, to_ns INTEGER NOT NULL, fetched_at_ns INTEGER NOT NULL,
  PRIMARY KEY (symbol, timeframe, from_ns)
);

CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL);
```

- `PutBars`: single txn, `INSERT INTO bars ... ON CONFLICT(symbol,timeframe,t)
  DO UPDATE SET o=excluded.o, ...`.
- `Bars`: `SELECT t,o,h,l,c,v,n,vw FROM bars WHERE symbol=? AND timeframe=? AND
  t BETWEEN ? AND ? ORDER BY t`.
- `Intervals`: `SELECT from_ns,to_ns,fetched_at_ns FROM fetched_ranges WHERE
  symbol=? AND timeframe=?`.
- `PutIntervals`: single txn — `DELETE ... WHERE symbol=? AND timeframe=?` then
  bulk-insert the coalesced set.
- Migration: create tables if absent; set `schema_meta.version=1`. Future
  versions bump and migrate.

## 7. Concurrency

- **Keyed singleflight** in the core: a per-`(symbol,timeframe)` lock serializes
  same-key `Get`s so concurrent callers cannot double-fetch the same gap (which
  also saves calls). Different keys proceed in parallel.
- Each adapter is internally safe: memRepo via its mutex; sqlitestore via
  SQLite's single-writer + WAL readers and `busy_timeout`.

## 8. Configuration (env vars)

| Var | Default | Meaning |
|-----|---------|---------|
| `STORAGE` | `memory` | `memory` or `disk`. `disk` selects the SQLite backend. |
| `STORAGE_PATH` | `./data/cache.db` | SQLite file path (used only when `STORAGE=disk`); parent dir auto-created. |

`config.Load` gains `StorageBackend` (validated to `memory`|`disk`, else error)
and `StoragePath`. Documented in `.env.example`. No CLI flags. Docker note:
mount `STORAGE_PATH` on a volume for cross-restart persistence.

## 9. Error Handling

- **Startup:** `STORAGE=disk` but the DB cannot be opened/created → **fail fast**
  (error from `main` → non-zero exit with a clear message). No silent fallback to
  memory — the user chose disk.
- **Read path** (`Bars`/`Intervals` error) → `observability.RecordError(ctx,
  ComponentStore, KindInternal, err)` and return the error.
- **Persist path** (`PutBars`/`PutIntervals` fails after a successful fetch) →
  serve the request from the freshly-fetched bars (held in memory for this call),
  `RecordError(store, internal)`, do **not** fail the request. The cache simply
  didn't persist this time.
- **Fetch errors** are handled in the alpaca client and returned by the store as
  today.

## 10. Observability

- Keep the existing `store.Get` span and `store.cache.hits`/`store.cache.misses`
  counters. Semantics: **hit** = served with no fetch (fully covered); **miss** =
  at least one range fetched. Add span attributes for planned-fetch range count
  and returned bar count. Errors via the existing `observability` helper.

## 11. Blast Radius (existing code)

- `internal/store/store.go` — rewritten around `Repository` + `coverage` policy.
  **`Get(ctx, symbol, timeframe, start, end) → []marketdata.Bar` unchanged**, so
  `httpapi.barSource` and the poller are untouched. `New` gains `repo` and
  `liveHorizon` params.
- `internal/ranges` — add `LiveHorizonForTimeframe`.
- `internal/httpapi/handlers.go` — drop the now-redundant `ranges.Slice(bars,
  start)` after `Get` (the engine already returns `[start,end]`). Response shape
  unchanged.
- `internal/config` — add `StorageBackend`/`StoragePath` + validation +
  `.env.example`.
- `main.go` — build the chosen repo (memRepo, or `sqlitestore.Open(path)`), pass
  to `store.New`, `defer repo.Close()`.
- `go.mod` — add `modernc.org/sqlite`.

## 12. Testing

- **`coverage_test.go`** (pure, injected `now`): `Coalesce` and `Plan` — gaps
  only; stale-tail refetch; immutable history not refetched; empty range
  recorded; sliver merge; backfill gap.
- **`storetest.RunRepositoryContract(t, repo)`** — one conformance suite run
  against **both** memRepo and sqlitestore: bars/intervals round-trip,
  range-query correctness, upsert idempotency, empty-range handling. Enforces
  LSP/symmetry across adapters.
- **Store policy test** — fake `Repository` + fake `FetchFunc`: fetches only
  planned ranges, coalesces, refetches only the stale tail, records empty
  intervals, no double-fetch under concurrent same-key `Get` (keyed lock), and
  the persist-failure degradation path.
- **sqlitestore test** — `t.TempDir()` DB: schema creation, WAL, upsert, range
  query.
- `make test` (`go test ./... -race`) stays green; the contract runs under
  `-race`.

## 13. Out of Scope (non-goals)

- Cross-process / shared cache (single-process only).
- Larger-than-memory working set beyond what SQLite naturally streams.
- Split/dividend-adjusted data invalidation (we use raw bars; note that adjusted
  data would retroactively rewrite history and need cache invalidation).
- Cache eviction/size caps (bounded in practice by symbols × timeframes ×
  history; revisit if it grows).
- Docker wiring of the volume (documented, not implemented here).
- CLI flags (env vars only).
