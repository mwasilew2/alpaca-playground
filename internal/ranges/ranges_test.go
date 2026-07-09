package ranges

import (
	"testing"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
)

func TestResolve_AllRanges(t *testing.T) {
	want := map[Range]string{
		R10m: "1Min", R1h: "1Min", R1d: "5Min", R1w: "1Hour",
		R1mo: "1Hour", R1y: "1Day", R5y: "1Week", RAll: "1Month",
	}
	for r, tf := range want {
		spec, err := Resolve(r)
		if err != nil {
			t.Fatalf("Resolve(%s) error: %v", r, err)
		}
		if spec.Timeframe != tf {
			t.Errorf("Resolve(%s).Timeframe = %q, want %q", r, spec.Timeframe, tf)
		}
	}
}

func TestResolve_Unknown(t *testing.T) {
	if _, err := Resolve("bogus"); err == nil {
		t.Fatal("expected error for unknown range")
	}
}

func TestSpecStart_AllUsesEpoch(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	spec, _ := Resolve(RAll)
	if got := spec.Start(now); !got.Equal(time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all Start = %v, want 2016-01-01", got)
	}
	spec1h, _ := Resolve(R1h)
	if got := spec1h.Start(now); !got.Equal(now.Add(-time.Hour)) {
		t.Errorf("1h Start = %v, want now-1h", got)
	}
}

func TestSlice(t *testing.T) {
	base := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	bars := []marketdata.Bar{
		{T: base}, {T: base.Add(time.Minute)}, {T: base.Add(2 * time.Minute)},
	}
	got := Slice(bars, base.Add(time.Minute))
	if len(got) != 2 || !got[0].T.Equal(base.Add(time.Minute)) {
		t.Errorf("Slice returned %d bars, first %v", len(got), got[0].T)
	}
	if len(Slice(bars, base.Add(time.Hour))) != 0 {
		t.Error("expected empty slice for start after all bars")
	}
}

func TestTTLAndLiveTimeframes(t *testing.T) {
	if TTLForTimeframe("1Min") >= TTLForTimeframe("1Day") {
		t.Error("intraday TTL should be shorter than daily TTL")
	}
	live := LiveTimeframes()
	if len(live) != 3 || live[0] != "1Min" || live[1] != "5Min" || live[2] != "1Hour" {
		t.Errorf("LiveTimeframes = %v", live)
	}
}
