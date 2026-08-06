package ranges

import (
	"testing"
	"time"
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

func TestTTLAndLiveTimeframes(t *testing.T) {
	if TTLForTimeframe("1Min") >= TTLForTimeframe("1Day") {
		t.Error("intraday TTL should be shorter than daily TTL")
	}
	live := LiveTimeframes()
	if len(live) != 3 || live[0] != "1Min" || live[1] != "5Min" || live[2] != "1Hour" {
		t.Errorf("LiveTimeframes = %v", live)
	}
}
