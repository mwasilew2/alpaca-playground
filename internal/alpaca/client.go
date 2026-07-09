// Package alpaca wraps the generated Alpaca market-data OpenAPI client with
// authentication, feed selection, pagination, and bounded retry-on-429
// behavior. It is the only package that imports gen/oapi.
package alpaca

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/mwasilew2/alpaca-playground/gen/oapi"
	"github.com/mwasilew2/alpaca-playground/internal/marketdata"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Client wraps the generated Alpaca market-data client with auth, feed, and the
// domain GetBars method. It is the only package that imports gen/oapi.
type Client struct {
	oc         *oapi.ClientWithResponses
	feed       oapi.StockHistoricalFeed
	maxRetries int
	backoff    func(attempt int) time.Duration
}

// New builds a Client. hc is the underlying *http.Client (a nil hc gets a
// default with a 30s timeout); its transport is wrapped with otelhttp so
// outbound calls are traced/metered.
func New(baseURL, key, secret, feed string, hc *http.Client) (*Client, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	traced := *hc
	traced.Transport = otelhttp.NewTransport(hc.Transport)

	auth := func(ctx context.Context, req *http.Request) error {
		req.Header.Set("APCA-API-KEY-ID", key)
		req.Header.Set("APCA-API-SECRET-KEY", secret)
		return nil
	}
	oc, err := oapi.NewClientWithResponses(baseURL,
		oapi.WithHTTPClient(&traced),
		oapi.WithRequestEditorFn(auth),
	)
	if err != nil {
		return nil, err
	}
	return &Client{
		oc:         oc,
		feed:       oapi.StockHistoricalFeed(feed),
		maxRetries: 4,
		backoff:    func(attempt int) time.Duration { return time.Duration(attempt) * 500 * time.Millisecond },
	}, nil
}

// GetBars fetches all bars for one symbol/timeframe in [start,end], following
// pagination and retrying on HTTP 429.
func (c *Client) GetBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	var out []marketdata.Bar
	var pageToken *string

	for {
		params := &oapi.StockBarsParams{
			Symbols:   symbol,
			TimeFrame: timeframe,
			Start:     &start,
			End:       &end,
			Feed:      &c.feed,
			PageToken: pageToken,
		}

		resp, err := c.doWithRetry(ctx, params)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			return nil, fmt.Errorf("alpaca stock bars: unexpected status %d", resp.StatusCode())
		}

		for _, sb := range resp.JSON200.Bars[symbol] {
			out = append(out, convert(sb))
		}

		if resp.JSON200.NextPageToken == nil || *resp.JSON200.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.JSON200.NextPageToken
	}
}

func (c *Client) doWithRetry(ctx context.Context, params *oapi.StockBarsParams) (*oapi.StockBarsResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.backoff(attempt)):
			}
		}
		resp, err := c.oc.StockBarsWithResponse(ctx, params)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode() == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("alpaca rate limited (429)")
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("alpaca request failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

func convert(sb oapi.StockBar) marketdata.Bar {
	return marketdata.Bar{
		T:  sb.Timestamp,
		O:  sb.Open,
		H:  sb.High,
		L:  sb.Low,
		C:  sb.Close,
		V:  sb.Volume,
		N:  sb.TradeCount,
		VW: sb.VWAP,
	}
}
