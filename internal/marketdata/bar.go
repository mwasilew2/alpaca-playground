package marketdata

import "time"

// Bar is a single OHLCV aggregate for one symbol over one timeframe interval.
type Bar struct {
	T  time.Time `json:"t"`  // bar start timestamp
	O  float64   `json:"o"`  // open
	H  float64   `json:"h"`  // high
	L  float64   `json:"l"`  // low
	C  float64   `json:"c"`  // close
	V  int64     `json:"v"`  // volume
	N  int64     `json:"n"`  // trade count
	VW float64   `json:"vw"` // volume-weighted average price
}
