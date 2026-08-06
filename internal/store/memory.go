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
