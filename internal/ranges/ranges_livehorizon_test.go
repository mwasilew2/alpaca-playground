package ranges

import "testing"

func TestLiveHorizonForTimeframe(t *testing.T) {
	for _, tf := range []string{"1Min", "5Min", "1Hour", "1Day", "1Week", "1Month"} {
		if lh := LiveHorizonForTimeframe(tf); lh < TTLForTimeframe(tf) {
			t.Errorf("liveHorizon(%s)=%v should be >= ttl %v", tf, lh, TTLForTimeframe(tf))
		}
	}
	if LiveHorizonForTimeframe("bogus") != 24*60*60*1e9 {
		t.Error("default live horizon should be 24h")
	}
}
