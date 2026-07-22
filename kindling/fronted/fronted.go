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
	tracerName = "github.com/getlantern/radiance/kindling/fronted"
	// configURL is the domainfront repo's daily-regenerated fronted.yaml.gz. The
	// old getlantern/fronted copy went stale when the masquerades cron's target
	// moved during the domainfront fork (last update 2026-06-05).
	configURL = "https://raw.githubusercontent.com/getlantern/domainfront/refs/heads/main/fronted.yaml.gz"
	// initialFetchTime bounds the background cache-warming fetch of configURL.
	// It no longer gates startup — startup boots on the cached/embedded config —
	// so a long timeout here is harmless where configURL is blocked.
	initialFetchTime = 30 * time.Second
	// configCacheName holds the last successfully fetched config so the next
	// startup can bootstrap when configURL is unreachable.
	configCacheName = "fronted_config.yaml.gz"
	// maxConfigBytes caps the size of the gzipped fronted config we'll accept
	// from the network. The real file is ~500 KB; this leaves room for growth
	// while bounding memory if a misconfigured or compromised endpoint serves
	// an unbounded body.
	maxConfigBytes = 10 << 20 // 10 MiB
)

// embeddedConfig is the last-resort bootstrap when the on-disk config cache is
// absent or unparseable — typical case is a fresh install with
// raw.githubusercontent.com blocked from the very first boot.
//
//go:embed fronted.yaml.gz
var embeddedConfig []byte

type bypassDialer struct{}

func (bypassDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return bypass.DialContext(ctx, network, addr)
}

// NewFronted builds a domainfront.Client for use with kindling's
// WithDomainFronting option. The caller owns the returned *Client and is
// responsible for calling Close() to shut down background goroutines.
//
// Startup does no network I/O: it bootstraps on the on-disk config cache (from
// a prior run) and then the embedded copy. configURL lives on
// raw.githubusercontent.com, which is blocked in some regions (e.g. China), and
// fetching it synchronously here stalled radiance init for the full
// initialFetchTime on every cold start there. The live config is refreshed off
// the critical path instead — domainfront's own config updater (WithConfigURL)
// applies it to the in-memory front pool, and a background goroutine warms the
// on-disk cache for the next cold start.
func NewFronted(ctx context.Context, cacheFile string, logWriter io.Writer) (*domainfront.Client, error) {
	_, span := otel.Tracer(tracerName).Start(ctx, "NewFronted")
	defer span.End()

	smartClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, configURL)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("build smart HTTP client: %w", err)
	}

	configCache := filepath.Join(filepath.Dir(cacheFile), configCacheName)
	cfg, err := loadBootstrapConfig(configCache)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("load bootstrap fronted config: %w", err)
	}

	opts := []domainfront.Option{
		domainfront.WithCacheFile(cacheFile),
		domainfront.WithDialer(bypassDialer{}),
		domainfront.WithLogger(slog.Default()),
		domainfront.WithConfigURL(configURL),
		domainfront.WithHTTPClient(smartClient),
	}
	client, err := domainfront.New(ctx, cfg, opts...)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	// Warm the on-disk cache so the next cold start bootstraps on a fresh
	// config even if configURL is unreachable then. Best-effort and off the
	// critical path: boot already succeeded on the cached copy, and the
	// in-memory pool is kept current by domainfront's own updater.
	go func() {
		if err := fetchAndCacheConfig(ctx, smartClient, configURL, configCache); err != nil {
			slog.Debug("background fronted-config cache refresh failed", "err", err)
		}
	}()

	return client, nil
}

// loadBootstrapConfig returns the startup config with no network I/O,
// preferring the on-disk cache over the embedded copy. It errors only when the
// embedded copy is itself unparseable.
func loadBootstrapConfig(path string) (*domainfront.Config, error) {
	if path != "" {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			if cfg, err := domainfront.ParseConfigFromReader(f); err == nil {
				slog.Debug("bootstrapped fronted config from on-disk cache", "path", path)
				return cfg, nil
			} else {
				slog.Warn("failed to parse on-disk fronted config, using embedded", "path", path, "err", err)
			}
		}
	}
	cfg, err := domainfront.ParseConfigFromReader(bytes.NewReader(embeddedConfig))
	if err != nil {
		return nil, fmt.Errorf("embedded fronted config parse failed: %w", err)
	}
	slog.Debug("bootstrapped fronted config from embedded copy")
	return cfg, nil
}

// fetchAndCacheConfig fetches the live config and, when it parses cleanly,
// persists it to configCache for the next cold start. It does no fallback:
// callers run it off the critical path and ignore failures, since startup has
// already succeeded on the bootstrap config.
func fetchAndCacheConfig(ctx context.Context, httpClient *http.Client, url, configCache string) error {
	fetchCtx, cancel := context.WithTimeout(ctx, initialFetchTime)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, url)
	}

	// Cap reads to maxConfigBytes + 1 so we can detect when the body exceeds
	// the limit (vs. a body that happens to be exactly maxConfigBytes long).
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxConfigBytes {
		return fmt.Errorf("response body exceeds %d bytes", maxConfigBytes)
	}
	// Validate before persisting so a corrupt body never poisons the cache.
	if _, err := domainfront.ParseConfigFromReader(bytes.NewReader(body)); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if configCache != "" {
		if err := atomicfile.WriteFile(configCache, body, fileperm.File); err != nil {
			return fmt.Errorf("persist config cache %s: %w", configCache, err)
		}
	}
	return nil
}
