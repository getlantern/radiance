package fronted

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/getlantern/domainfront"
	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/kindling/smart"
)

const (
	tracerName       = "github.com/getlantern/radiance/kindling/fronted"
	configURL        = "https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz"
	initialFetchTime = 30 * time.Second
	// configCacheName sits next to the runtime-state cacheFile. Holds the
	// last successfully fetched fronted.yaml.gz so the next startup can
	// bootstrap when raw.githubusercontent.com is unreachable.
	configCacheName = "fronted_config.yaml.gz"
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
// (bypassing DNS- and SNI-level blocking of raw.githubusercontent.com) with
// a 30s timeout; subsequent updates happen on domainfront's internal 12h
// loop using the same smart-dialer-backed HTTP client. On fetch failure we
// fall back to the last successfully fetched config persisted next to
// cacheFile, so a blocked or offline first boot after a prior good fetch
// still initializes.
func NewFronted(ctx context.Context, cacheFile string, logWriter io.Writer) (*domainfront.Client, error) {
	_, span := otel.Tracer(tracerName).Start(ctx, "NewFronted")
	defer span.End()

	smartClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, configURL)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("build smart HTTP client: %w", err)
	}

	configCache := filepath.Join(filepath.Dir(cacheFile), configCacheName)
	cfg, err := fetchInitialConfig(ctx, smartClient, configCache)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("fetch initial fronted config: %w", err)
	}

	opts := []domainfront.Option{
		domainfront.WithCacheFile(cacheFile),
		domainfront.WithDialer(bypassDialer{}),
		domainfront.WithLogger(slog.Default()),
		domainfront.WithConfigURL(configURL),
		domainfront.WithHTTPClient(smartClient),
	}

	return domainfront.New(ctx, cfg, opts...)
}

// fetchInitialConfig fetches the fronted config over HTTP and, on success,
// persists the raw gzipped bytes to configCache for offline fallback. If the
// fetch fails but configCache exists from a prior successful fetch, the
// cached bytes are parsed and returned instead.
func fetchInitialConfig(ctx context.Context, httpClient *http.Client, configCache string) (*domainfront.Config, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, initialFetchTime)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, configURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return loadCachedConfig(configCache, fmt.Errorf("fetch %s: %w", configURL, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return loadCachedConfig(configCache, fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, configURL))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return loadCachedConfig(configCache, fmt.Errorf("read response: %w", err))
	}
	cfg, err := domainfront.ParseConfigFromReader(bytes.NewReader(body))
	if err != nil {
		return loadCachedConfig(configCache, fmt.Errorf("parse response: %w", err))
	}
	// Persist after a known-good parse so we don't cache unparseable bytes.
	if configCache != "" {
		if err := os.WriteFile(configCache, body, 0o644); err != nil {
			slog.Warn("failed to persist fronted config cache", "path", configCache, "err", err)
		}
	}
	return cfg, nil
}

// loadCachedConfig attempts to read a previously persisted fronted config.
// Returns fetchErr if no cache exists or it can't be parsed, so callers see
// the original fetch failure when we have nothing to fall back on.
func loadCachedConfig(path string, fetchErr error) (*domainfront.Config, error) {
	if path == "" {
		return nil, fetchErr
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fetchErr
	}
	defer f.Close()
	cfg, err := domainfront.ParseConfigFromReader(f)
	if err != nil {
		slog.Warn("failed to parse cached fronted config, using fetch error", "path", path, "err", err)
		return nil, fetchErr
	}
	slog.Warn("using cached fronted config", "path", path, "fetch_err", fetchErr)
	return cfg, nil
}
