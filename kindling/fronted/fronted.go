package fronted

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/getlantern/domainfront"
	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/kindling/smart"
	"go.opentelemetry.io/otel"
)

const (
	tracerName       = "github.com/getlantern/radiance/kindling/fronted"
	configURL        = "https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz"
	initialFetchTime = 30 * time.Second
)

// bypassDialer adapts bypass.DialContext (a function) to the domainfront.Dialer
// interface.
type bypassDialer struct{}

func (bypassDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return bypass.DialContext(ctx, network, addr)
}

// NewFronted builds a domainfront.Client for use with kindling's
// WithDomainFronting option. The caller owns the returned *Client and is
// responsible for calling Close() to shut down background goroutines.
//
// The initial fronted.yaml.gz is fetched synchronously via the smart dialer
// (bypassing DNS-level SNI-level blocking of raw.githubusercontent.com) with
// a 30s timeout; subsequent updates happen on domainfront's internal 12h
// loop using the same smart-dialer-backed HTTP client.
func NewFronted(ctx context.Context, cacheFile string, logWriter io.Writer) (*domainfront.Client, error) {
	_, span := otel.Tracer(tracerName).Start(ctx, "NewFronted")
	defer span.End()

	smartClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, configURL)
	if err != nil {
		span.RecordError(err)
		slog.Error("failed to build http client with smart HTTP transport", slog.Any("error", err))
	}

	cfg, err := fetchInitialConfig(ctx, smartClient)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("fetch initial fronted config: %w", err)
	}

	opts := []domainfront.Option{
		domainfront.WithCacheFile(cacheFile),
		domainfront.WithDialer(bypassDialer{}),
		domainfront.WithLogger(slog.Default()),
		domainfront.WithConfigURL(configURL),
	}
	if smartClient != nil {
		opts = append(opts, domainfront.WithHTTPClient(smartClient))
	}

	return domainfront.New(ctx, cfg, opts...)
}

func fetchInitialConfig(ctx context.Context, smartClient *http.Client) (*domainfront.Config, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, initialFetchTime)
	defer cancel()

	httpClient := smartClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, configURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", configURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, configURL)
	}
	return domainfront.ParseConfigFromReader(resp.Body)
}
