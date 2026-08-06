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
