package poller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/store"
)

func TestPoller_RefreshesAllSymbolsAndTimeframes(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		mu.Lock()
		seen[sym+"|"+tf]++
		mu.Unlock()
		return nil, nil
	}
	st := store.New(fetch, store.NewMemRepository(), func(string) time.Duration { return 0 }, func(string) time.Duration { return time.Hour }) // TTL 0 => always refetch
	p := New(st, []string{"AAPL", "TSLA"}, []string{"1Min", "5Min"}, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	// Wait for the immediate first refresh to populate all 4 combos.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d combos refreshed, want 4", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestPoller_ErrorDoesNotStopLoop(t *testing.T) {
	var mu sync.Mutex
	count := 0
	fetch := func(ctx context.Context, sym, tf string, start, end time.Time) ([]marketdata.Bar, error) {
		mu.Lock()
		count++
		mu.Unlock()
		if sym == "BAD" {
			return nil, errors.New("boom")
		}
		return nil, nil
	}
	st := store.New(fetch, store.NewMemRepository(), func(string) time.Duration { return 0 }, func(string) time.Duration { return time.Hour })
	p := New(st, []string{"BAD", "GOOD"}, []string{"1Min"}, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if count < 2 {
		t.Errorf("expected both symbols attempted despite error, got %d", count)
	}
}
