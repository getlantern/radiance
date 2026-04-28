package fronted

import (
	"bytes"
	"context"
	_ "embed"
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
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
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
	// maxConfigBytes caps the size of the gzipped fronted config we'll accept
	// from the network. The real file is ~500 KB; this leaves room for growth
	// while bounding memory if a misconfigured or compromised endpoint serves
	// an unbounded body.
	maxConfigBytes = 10 << 20 // 10 MiB
)

// embeddedConfig is the last-resort fallback when both the live fetch and
// the local config cache fail — typical case is a fresh install with
// raw.githubusercontent.com blocked from the very first boot. domainfront
// itself doesn't embed a config (the old getlantern/fronted package did),
// so the caller owns the embedded copy. Refresh periodically from
// https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz
// and commit alongside this file.
//
//go:embed fronted.yaml.gz
var embeddedConfig []byte

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

	// Cap reads to maxConfigBytes + 1 so we can detect when the body exceeds
	// the limit (vs. a body that happens to be exactly maxConfigBytes long).
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes+1))
	if err != nil {
		return loadCachedConfig(configCache, fmt.Errorf("read response: %w", err))
	}
	if len(body) > maxConfigBytes {
		return loadCachedConfig(configCache, fmt.Errorf("response body exceeds %d bytes", maxConfigBytes))
	}
	cfg, err := domainfront.ParseConfigFromReader(bytes.NewReader(body))
	if err != nil {
		return loadCachedConfig(configCache, fmt.Errorf("parse response: %w", err))
	}
	// Persist after a known-good parse so we don't cache unparseable bytes.
	if configCache != "" {
		if err := atomicfile.WriteFile(configCache, body, fileperm.File); err != nil {
			slog.Warn("failed to persist fronted config cache", "path", configCache, "err", err)
		}
	}
	return cfg, nil
}

// loadCachedConfig tries, in order: the on-disk config cache (from a prior
// successful fetch) and then the embedded config. Returns fetchErr only if
// both fallbacks fail to parse — so a clean install with the live fetch
// blocked still boots on the embedded copy.
func loadCachedConfig(path string, fetchErr error) (*domainfront.Config, error) {
	if path != "" {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			if cfg, err := domainfront.ParseConfigFromReader(f); err == nil {
				slog.Warn("using cached fronted config", "path", path, "fetch_err", fetchErr)
				return cfg, nil
			} else {
				slog.Warn("failed to parse on-disk fronted config, trying embedded", "path", path, "err", err)
			}
		}
	}
	cfg, err := domainfront.ParseConfigFromReader(bytes.NewReader(embeddedConfig))
	if err != nil {
		return nil, fmt.Errorf("embedded fronted config parse failed: %w (original fetch error: %v)", err, fetchErr)
	}
	slog.Warn("using embedded fronted config", "fetch_err", fetchErr)
	return cfg, nil
}
