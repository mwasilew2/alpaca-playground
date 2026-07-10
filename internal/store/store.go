package store

import (
	"context"
	"sync"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// FetchFunc loads bars for one symbol/timeframe over [start,end] from upstream.
type FetchFunc func(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error)

type entry struct {
	bars      []marketdata.Bar
	start     time.Time // earliest start this entry covers
	fetchedAt time.Time
}

// Store is a thread-safe read-through cache keyed by (symbol, timeframe).
type Store struct {
	mu    sync.Mutex
	data  map[string]entry
	fetch FetchFunc
	ttl   func(timeframe string) time.Duration
	now   func() time.Time

	tracer trace.Tracer
	hits   metric.Int64Counter
	misses metric.Int64Counter
}

// New creates a Store. ttl maps a timeframe to its freshness window.
func New(fetch FetchFunc, ttl func(string) time.Duration) *Store {
	m := otel.Meter("alpaca-playground/store")
	hits, _ := m.Int64Counter("store.cache.hits")
	misses, _ := m.Int64Counter("store.cache.misses")
	return &Store{
		data:   make(map[string]entry),
		fetch:  fetch,
		ttl:    ttl,
		now:    time.Now,
		tracer: otel.Tracer("alpaca-playground/store"),
		hits:   hits,
		misses: misses,
	}
}

func key(symbol, timeframe string) string { return symbol + "|" + timeframe }

// Get returns bars for the symbol/timeframe covering [start,end]. It serves a
// fresh, covering cache entry, otherwise fetches upstream and caches the result.
func (s *Store) Get(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	ctx, span := s.tracer.Start(ctx, "store.Get", trace.WithAttributes(
		attribute.String("symbol", symbol),
		attribute.String("timeframe", timeframe),
	))
	defer span.End()

	k := key(symbol, timeframe)
	now := s.now()

	s.mu.Lock()
	e, ok := s.data[k]
	fresh := ok && now.Sub(e.fetchedAt) < s.ttl(timeframe) && !e.start.After(start)
	if fresh {
		bars := e.bars
		s.mu.Unlock()
		span.SetAttributes(attribute.Bool("cache.hit", true))
		if s.hits != nil {
			s.hits.Add(ctx, 1)
		}
		return bars, nil
	}
	s.mu.Unlock()

	span.SetAttributes(attribute.Bool("cache.hit", false))
	// Fetch happens outside the lock; two concurrent misses for the same key may both fetch. Accepted tradeoff (no singleflight).
	if s.misses != nil {
		s.misses.Add(ctx, 1)
	}
	bars, err := s.fetch(ctx, symbol, timeframe, start, end)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream fetch failed")
		return nil, err
	}

	s.mu.Lock()
	s.data[k] = entry{bars: bars, start: start, fetchedAt: now}
	s.mu.Unlock()
	return bars, nil
}
