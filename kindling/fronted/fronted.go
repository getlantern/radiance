package fronted

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"

	"go.opentelemetry.io/otel"

	"github.com/getlantern/domainfront"
	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/kindling/smart"
)

const (
	tracerName = "github.com/getlantern/radiance/kindling/fronted"
	// configURL is the domainfront repo's daily-regenerated fronted.yaml.gz. The
	// old getlantern/fronted copy went stale when the masquerades cron's target
	// moved during the domainfront fork (last update 2026-06-05).
	configURL = "https://raw.githubusercontent.com/getlantern/domainfront/refs/heads/main/fronted.yaml.gz"
	// configCacheName is where domainfront persists the last successfully fetched
	// config so the next start can bootstrap when configURL is unreachable.
	configCacheName = "fronted_config.yaml.gz"
)

// embeddedConfig seeds domainfront on a fresh install (no persisted cache yet),
// e.g. with raw.githubusercontent.com blocked from the very first boot.
//
//go:embed fronted.yaml.gz
var embeddedConfig []byte

type bypassDialer struct{}

func (bypassDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return bypass.DialContext(ctx, network, addr)
}

// NewFronted builds a domainfront.Client for use with kindling's
// WithDomainFronting option. The caller owns the returned *Client and is
// responsible for calling Close() to shut down its background goroutines.
//
// Startup does no blocking network I/O: it seeds domainfront with the embedded
// config and lets domainfront own the live config lifecycle. configURL lives on
// raw.githubusercontent.com, which is blocked in some regions (e.g. China);
// fetching it synchronously here stalled radiance init for the full fetch
// timeout on every cold start there. domainfront's config updater
// (WithConfigURL) now fetches it off the critical path, persists it
// (WithConfigCacheFile), and bootstraps from that persisted copy on the next
// start in preference to the embedded seed.
func NewFronted(ctx context.Context, cacheFile string, logWriter io.Writer) (*domainfront.Client, error) {
	_, span := otel.Tracer(tracerName).Start(ctx, "NewFronted")
	defer span.End()

	smartClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, configURL)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("build smart HTTP client: %w", err)
	}

	seed, err := domainfront.ParseConfigFromReader(bytes.NewReader(embeddedConfig))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("parse embedded fronted config: %w", err)
	}

	opts := []domainfront.Option{
		domainfront.WithCacheFile(cacheFile),
		domainfront.WithConfigCacheFile(filepath.Join(filepath.Dir(cacheFile), configCacheName)),
		domainfront.WithDialer(bypassDialer{}),
		domainfront.WithLogger(slog.Default()),
		domainfront.WithConfigURL(configURL),
		domainfront.WithHTTPClient(smartClient),
	}
	return domainfront.New(ctx, seed, opts...)
}
