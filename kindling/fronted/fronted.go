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
	// mirrorConfigURL serves the same fronted.yaml.gz via jsDelivr from a
	// non-getlantern account, reachable where raw.githubusercontent.com is
	// blocked (e.g. China) — jsDelivr bans the getlantern org, so the mirror
	// lives elsewhere. Raced against configURL; first valid response wins.
	mirrorConfigURL = "https://cdn.jsdelivr.net/gh/firetweet/domainfront@main/fronted.yaml.gz"
	// configCacheName is where domainfront persists the last successfully fetched
	// config so the next start can bootstrap when the config hosts are unreachable.
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
// config and lets domainfront own the live config lifecycle. The primary source
// (configURL) is on raw.githubusercontent.com, blocked in some regions (e.g.
// China), so it's raced against a jsDelivr mirror (mirrorConfigURL) — first
// valid response wins. domainfront's config updater (WithConfigURL) fetches them
// off the critical path, persists the result (WithConfigCacheFile), and
// bootstraps from that persisted copy on the next start in preference to the
// embedded seed. The smart HTTP client is tuned for both hosts.
func NewFronted(ctx context.Context, cacheFile string, logWriter io.Writer) (*domainfront.Client, error) {
	_, span := otel.Tracer(tracerName).Start(ctx, "NewFronted")
	defer span.End()

	smartClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, configURL, mirrorConfigURL)
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
		domainfront.WithConfigURL(configURL, mirrorConfigURL),
		domainfront.WithHTTPClient(smartClient),
	}
	return domainfront.New(ctx, seed, opts...)
}
