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
