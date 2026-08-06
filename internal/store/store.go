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
