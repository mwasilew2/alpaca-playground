package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/observability"
	"github.com/mwasilew2/alpaca-playground/internal/store"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// windowFor is how much history the poller keeps warm per intraday timeframe.
func windowFor(tf string) time.Duration {
	switch tf {
	case "1Min":
		return time.Hour
	case "5Min":
		return 24 * time.Hour
	case "1Hour":
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// Poller periodically refreshes watchlist data through the store.
type Poller struct {
	store      *store.Store
	symbols    []string
	timeframes []string
	interval   time.Duration
	now        func() time.Time

	tracer trace.Tracer
	ticks  metric.Int64Counter
}

// New builds a Poller for the given symbols and (live) timeframes.
func New(s *store.Store, symbols, timeframes []string, interval time.Duration) *Poller {
	m := otel.Meter("alpaca-playground/poller")
	ticks, _ := m.Int64Counter("poller.ticks")
	return &Poller{
		store:      s,
		symbols:    symbols,
		timeframes: timeframes,
		interval:   interval,
		now:        time.Now,
		tracer:     otel.Tracer("alpaca-playground/poller"),
		ticks:      ticks,
	}
}

// Run refreshes immediately, then every interval, until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.refresh(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refresh(ctx)
		}
	}
}

func (p *Poller) refresh(ctx context.Context) {
	// The poller is not request-driven, so each tick starts its own root span;
	// the store/alpaca spans it triggers nest beneath it as one trace.
	ctx, span := p.tracer.Start(ctx, "poller.refresh")
	defer span.End()

	if p.ticks != nil {
		p.ticks.Add(ctx, 1)
	}
	now := p.now()
	for _, sym := range p.symbols {
		for _, tf := range p.timeframes {
			start := now.Add(-windowFor(tf))
			if _, err := p.store.Get(ctx, sym, tf, start, now); err != nil {
				span.SetStatus(codes.Error, "refresh error")
				observability.CountError(ctx, observability.ComponentPoller, observability.KindUpstream)
				slog.WarnContext(ctx, "poller refresh failed", "symbol", sym, "timeframe", tf, "err", err)
			}
		}
	}
}
