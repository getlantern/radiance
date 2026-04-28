// Package kindling provides a wrapper around the kindling library to create an HTTP client with
// various transports (domain fronting, AMP, DNS tunneling, proxyless) from a shared kindling instance.
package kindling

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/getlantern/kindling"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/proxy"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling/dnstt"
	"github.com/getlantern/radiance/kindling/fronted"
	"github.com/getlantern/radiance/traces"
)

var (
	k               kindling.Kindling
	initOnce        sync.Once
	stopUpdater     func()
	closeTransports []func() error
	// EnabledTransports is used for testing purposes for enabling/disabling kindling transports
	EnabledTransports = map[string]bool{
		"dnstt":     false,
		"amp":       true,
		"proxyless": true,
		"fronted":   true,
	}
	defaultTransportClone = http.DefaultTransport.(*http.Transport).Clone()

	// transport is the shared http.RoundTripper set once by initOnce.
	transport http.RoundTripper
)

// initKindling initializes the package-level kindling instance and shared
// transport.
func initKindling() {
	// Censorship-circumvention QA path: when OutboundSocksAddress is set,
	// every outbound HTTP dial goes through that SOCKS5 server. Kindling's
	// stacked transports (fronted/AMP/dnstt/proxyless) are skipped — the
	// SOCKS5 is providing egress, and kindling's per-transport internal
	// dialers don't expose an override hook today. As a result, when this
	// var is set we are testing "does the bandit/tunnel path work given a
	// reachable API channel" rather than the full anti-censorship stack.
	if addr, ok := env.Get(env.OutboundSocksAddress); ok && addr != "" {
		t, err := socksOnlyTransport(addr)
		if err != nil {
			slog.Error("invalid RADIANCE_OUTBOUND_SOCKS_ADDRESS, falling back to default transport", slog.Any("error", err))
			transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(defaultTransportClone))
			return
		}
		slog.Info("RADIANCE_OUTBOUND_SOCKS_ADDRESS set — routing all radiance HTTP through upstream SOCKS5", slog.String("addr", addr))
		transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(t))
		return
	}
	newK, err := NewKindling(settings.GetString(settings.DataPathKey))
	if err != nil {
		slog.Error("failed to create kindling client", slog.Any("error", err))
	}
	if newK != nil {
		k = newK
		transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(newK.NewHTTPClient().Transport))
	} else {
		slog.Warn("kindling unavailable, using default transport clone")
		transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(defaultTransportClone))
	}
}

// socksOnlyTransport returns an http.Transport that dials through the given
// SOCKS5 server for every connection.
func socksOnlyTransport(socksAddr string) (*http.Transport, error) {
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("building SOCKS5 dialer for %s: %w", socksAddr, err)
	}
	ctxDialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("SOCKS5 dialer does not support context")
	}
	t := defaultTransportClone.Clone()
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return ctxDialer.DialContext(ctx, network, address)
	}
	// Disable HTTP_PROXY env-based proxying — we route via DialContext instead.
	// (x/net/proxy's SOCKS5 sends the hostname to the upstream as ATYP=domain,
	// so DNS resolution also happens at the SOCKS5 server, no local leak.)
	t.Proxy = nil
	return t, nil
}

func Init() {
	go initOnce.Do(initKindling)
}

// HTTPClient returns an HTTP client whose transport blocks on first use
// until kindling is initialized.
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout:   common.DefaultHTTPTimeout,
		Transport: readyTransport{},
	}
}

// readyTransport blocks until initOnce has completed, then delegates to the
// shared transport.
type readyTransport struct{}

func (readyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	initOnce.Do(initKindling)
	return transport.RoundTrip(req)
}

// Close stops any in-flight config fetches and releases kindling transports.
func Close() error {
	if stopUpdater != nil {
		stopUpdater()
	}
	for _, c := range closeTransports {
		if err := c(); err != nil {
			slog.Error("failed to close kindling transport", slog.Any("error", err))
		}
	}
	return nil
}

// SetKindling installs a kindling instance for tests, bypassing the normal
// initialization path. Call it before any HTTPClient usage; otherwise
// initOnce will have already run and this call becomes a no-op.
func SetKindling(a kindling.Kindling) {
	initOnce.Do(func() {
		k = a
		if a != nil {
			transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(a.NewHTTPClient().Transport))
		} else {
			transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(defaultTransportClone))
		}
	})
}

const tracerName = "github.com/getlantern/radiance/kindling"

// NewKindling build a kindling client and bootstrap this package
func NewKindling(dataDir string) (kindling.Kindling, error) {
	logger := &slogWriter{Logger: slog.Default()}

	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"NewKindling",
		trace.WithAttributes(attribute.String("data_path", dataDir)),
	)
	defer span.End()

	if common.Stage() {
		// Disable domain fronting for stage environment to avoid hitting staging server issues due to fronted client failures.
		return kindling.NewKindling("radiance",
			kindling.WithPanicListener(reporting.PanicListener),
			kindling.WithLogWriter(logger),
			// Most endpoints use df.iantem.io, but for some historical reasons
			// "pro-server" calls still go to api.getiantem.org.
			kindling.WithProxyless("df.iantem.io", "api.getiantem.org", "api.staging.iantem.io"),
		)
	}

	closeTransports = []func() error{}
	kindlingOptions := []kindling.Option{
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(logger),
	}

	updaterCtx, cancel := context.WithCancel(ctx)
	if enabled := EnabledTransports["fronted"]; enabled {
		f, err := fronted.NewFronted(updaterCtx, reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), logger)
		if err != nil {
			slog.Error("failed to create fronted client", slog.Any("error", err))
			span.RecordError(err)
		}
		if f != nil {
			closeTransports = append(closeTransports, func() error { f.Close(); return nil })
			kindlingOptions = append(kindlingOptions, kindling.WithDomainFronting(f))
		}
	}

	if enabled := EnabledTransports["amp"]; enabled {
		ampClient, err := fronted.NewAMPClient(updaterCtx, dataDir, logger)
		if err != nil {
			slog.Error("failed to create amp client", slog.Any("error", err))
			span.RecordError(err)
		}
		if ampClient != nil {
			kindlingOptions = append(kindlingOptions, kindling.WithAMPCache(ampClient))
		}
	}

	if enabled := EnabledTransports["dnstt"]; enabled {
		dnsttOptions, err := dnstt.DNSTTOptions(updaterCtx, filepath.Join(dataDir, "dnstt.yml.gz"), logger)
		if err != nil {
			slog.Error("failed to create or load dnstt kindling options", slog.Any("error", err))
			span.RecordError(err)
		}
		if dnsttOptions != nil {
			closeTransports = append(closeTransports, dnsttOptions.Close)
			kindlingOptions = append(kindlingOptions, kindling.WithDNSTunnel(dnsttOptions))
		}
	}

	if enabled := EnabledTransports["proxyless"]; enabled {
		// Most endpoints use df.iantem.io, but for some historical reasons
		// "pro-server" calls still go to api.getiantem.org.
		kindlingOptions = append(kindlingOptions, kindling.WithProxyless("df.iantem.io", "api.getiantem.org"))
	}

	stopUpdater = cancel
	return kindling.NewKindling("radiance", kindlingOptions...)
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	// Convert the byte slice to a string and log it
	s := string(p)
	s = strings.TrimSpace(s)
	w.Info(s)
	return len(p), nil
}
