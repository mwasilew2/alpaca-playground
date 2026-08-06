package ranges

import (
	"fmt"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

// Range is a user-selectable chart time range.
type Range string

const (
	R10m Range = "10m"
	R1h  Range = "1h"
	R1d  Range = "1d"
	R1w  Range = "1w"
	R1mo Range = "1mo"
	R1y  Range = "1y"
	R5y  Range = "5y"
	RAll Range = "all"
)

// Spec is the resolved fetch plan for a range.
type Spec struct {
	Timeframe string        // Alpaca timeframe string, e.g. "1Min"
	Lookback  time.Duration // 0 means "all available history"
}

// epochStart bounds the "all" range; Alpaca stock history begins ~2016.
var epochStart = time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)

// Start returns the inclusive start time for this spec relative to now.
func (s Spec) Start(now time.Time) time.Time {
	if s.Lookback <= 0 {
		return epochStart
	}
	return now.Add(-s.Lookback)
}

const day = 24 * time.Hour

var table = map[Range]Spec{
	R10m: {Timeframe: "1Min", Lookback: 10 * time.Minute},
	R1h:  {Timeframe: "1Min", Lookback: time.Hour},
	R1d:  {Timeframe: "5Min", Lookback: day},
	R1w:  {Timeframe: "1Hour", Lookback: 7 * day},
	R1mo: {Timeframe: "1Hour", Lookback: 30 * day},
	R1y:  {Timeframe: "1Day", Lookback: 365 * day},
	R5y:  {Timeframe: "1Week", Lookback: 5 * 365 * day},
	RAll: {Timeframe: "1Month", Lookback: 0},
}

// Resolve maps a range to its fetch spec.
func Resolve(r Range) (Spec, error) {
	spec, ok := table[r]
	if !ok {
		return Spec{}, fmt.Errorf("unknown range %q", r)
	}
	return spec, nil
}

// Valid reports whether r is a known range.
func (r Range) Valid() bool {
	_, ok := table[r]
	return ok
}

// AllRanges returns every supported range (unspecified order).
func AllRanges() []Range {
	out := make([]Range, 0, len(table))
	for r := range table {
		out = append(out, r)
	}
	return out
}

// Slice returns the bars with T >= start. Input must be ascending by T.
func Slice(bars []marketdata.Bar, start time.Time) []marketdata.Bar {
	for i, b := range bars {
		if !b.T.Before(start) {
			return bars[i:]
		}
	}
	return nil
}

// TTLForTimeframe is the cache freshness window for a timeframe. Intraday data
// changes constantly; daily+ data changes at most once per day.
func TTLForTimeframe(tf string) time.Duration {
	switch tf {
	case "1Min":
		return 15 * time.Second
	case "5Min":
		return 30 * time.Second
	case "1Hour":
		return 5 * time.Minute
	case "1Day":
		return time.Hour
	case "1Week":
		return 12 * time.Hour
	case "1Month":
		return 24 * time.Hour
	default:
		return time.Minute
	}
}

// LiveTimeframes are the intraday timeframes the poller keeps warm.
func LiveTimeframes() []string {
	return []string{"1Min", "5Min", "1Hour"}
}

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
