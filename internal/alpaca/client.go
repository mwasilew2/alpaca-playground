// Package alpaca wraps the generated Alpaca market-data OpenAPI client with
// authentication, feed selection, pagination, and retry behavior. Retries
// (429/5xx/transport errors, honoring Retry-After) are delegated to
// hashicorp/go-retryablehttp at the transport layer. It is the only package
// that imports gen/oapi.
package alpaca

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mwasilew2/alpaca-playground/gen/oapi"
	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/observability"

	"github.com/hashicorp/go-retryablehttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Client wraps the generated Alpaca market-data client with auth, feed, and the
// domain GetBars method. It is the only package that imports gen/oapi.
type Client struct {
	oc     *oapi.ClientWithResponses
	feed   oapi.StockHistoricalFeed
	tracer trace.Tracer
	rc     *retryablehttp.Client // retained so retry timing can be tuned (e.g. in tests)
}

// New builds a Client. hc is the underlying *http.Client (a nil hc gets a
// default with a 30s timeout); its transport is wrapped with otelhttp so each
// attempt is traced/metered, and the whole thing is driven by a retryablehttp
// client that retries 429/5xx/transport errors with Retry-After-aware backoff.
func New(baseURL, key, secret, feed string, hc *http.Client) (*Client, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// Per-attempt HTTP client: trace/meter every try via otelhttp.
	inner := *hc
	inner.Transport = otelhttp.NewTransport(hc.Transport)

	rc := retryablehttp.NewClient()
	rc.HTTPClient = &inner
	rc.Logger = nil // logs/metrics are emitted via OTel; silence hclog's stderr output
	rc.RetryMax = 4
	rc.RetryWaitMin = 500 * time.Millisecond
	rc.RetryWaitMax = 5 * time.Second
	// On exhausted retries, pass the final response through so GetBars reports
	// its status code rather than a generic "giving up" error.
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler

	auth := func(ctx context.Context, req *http.Request) error {
		req.Header.Set("APCA-API-KEY-ID", key)
		req.Header.Set("APCA-API-SECRET-KEY", secret)
		return nil
	}
	oc, err := oapi.NewClientWithResponses(baseURL,
		oapi.WithHTTPClient(rc.StandardClient()),
		oapi.WithRequestEditorFn(auth),
	)
	if err != nil {
		return nil, err
	}
	return &Client{
		oc:     oc,
		feed:   oapi.StockHistoricalFeed(feed),
		tracer: otel.Tracer("alpaca-playground/alpaca"),
		rc:     rc,
	}, nil
}

// GetBars fetches all bars for one symbol/timeframe in [start,end], following
// pagination. Transient failures are retried transparently by the transport.
func (c *Client) GetBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	ctx, span := c.tracer.Start(ctx, "alpaca.GetBars", trace.WithAttributes(
		attribute.String("symbol", symbol),
		attribute.String("timeframe", timeframe),
	))
	defer span.End()

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

		resp, err := c.oc.StockBarsWithResponse(ctx, params)
		if err != nil {
			kind := observability.KindUpstream
			if errors.Is(err, context.DeadlineExceeded) {
				kind = observability.KindTimeout
			}
			observability.RecordError(ctx, observability.ComponentAlpaca, kind, err)
			return nil, err
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			err := fmt.Errorf("alpaca stock bars: unexpected status %d", resp.StatusCode())
			kind := observability.KindUpstream
			if resp.StatusCode() == http.StatusTooManyRequests {
				kind = observability.KindRateLimited
			}
			observability.RecordError(ctx, observability.ComponentAlpaca, kind, err)
			return nil, err
		}

		for _, sb := range resp.JSON200.Bars[symbol] {
			out = append(out, convert(sb))
		}

		if resp.JSON200.NextPageToken == nil || *resp.JSON200.NextPageToken == "" {
			span.SetAttributes(attribute.Int("bars.count", len(out)))
			return out, nil
		}
		pageToken = resp.JSON200.NextPageToken
	}
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
