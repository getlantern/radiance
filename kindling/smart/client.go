// Package smart contains a HTTP client with smart transport used by other
// methods to fetch config updates
package smart

import (
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"

	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/traces"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/getlantern/radiance/kindling/smart"

// DialerConfig is a copy of kindling's smart_dialer_config.yml with the
// `system: {}` DNS entry removed. The outline-sdk smart strategy rejects
// any base StreamDialer that isn't *transport.TCPDialer when the system
// resolver is selected, which the bypass dialer can't satisfy. DoH entries
// route every probe through the supplied StreamDialer instead, which is
// what we want anyway — system DNS uses OS routing tables and would loop
// back through the VPN TUN we're trying to bypass.
//
//go:embed smart_dialer_config.yml
var DialerConfig []byte

func NewHTTPClientWithSmartTransport(logWriter io.Writer, addresses ...string) (*http.Client, error) {
	// Extract the host from each URL; the smart dialer searches for a working
	// strategy against all of them, so a caller that races several config
	// sources (e.g. the GitHub config URL and a jsDelivr mirror) gets a transport
	// tuned for every host it may fetch from.
	domains := make([]string, 0, len(addresses))
	for _, address := range addresses {
		u, err := url.Parse(address)
		if err != nil {
			return nil, fmt.Errorf("failed to parse URL %q: %v", address, err)
		}
		// Hostname() drops any :port; skip entries with no host (e.g. a
		// scheme-less URL) so the dialer isn't tuned for an empty domain.
		if host := u.Hostname(); host != "" {
			domains = append(domains, host)
		}
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("no valid config host among %v", addresses)
	}
	trans, err := kindling.NewSmartHTTPTransportWithConfig(logWriter, DialerConfig, bypass.StreamDialer(), nil, domains...)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart HTTP transport: %v", err)
	}
	lz := &lazyDialingRoundTripper{
		smartTransportMu: sync.Mutex{},
		logWriter:        logWriter,
		domains:          domains,
	}
	if trans != nil {
		lz.smartTransport = trans
	}
	return &http.Client{Transport: traces.NewRoundTripper(lz)}, nil
}

// This is a lazy RoundTripper that allows radiance to start without an error
// when it's offline.
type lazyDialingRoundTripper struct {
	smartTransport   http.RoundTripper
	smartTransportMu sync.Mutex

	logWriter io.Writer
	domains   []string
}

// Make sure lazyDialingRoundTripper implements http.RoundTripper
var _ http.RoundTripper = (*lazyDialingRoundTripper)(nil)

func (lz *lazyDialingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer(tracerName).Start(
		req.Context(),
		"lazy_dialing_round_trip",
		trace.WithAttributes(
			// Keep the original "domain" key (first host) for existing tracing
			// queries; add "domains" for the full raced set. domains is always
			// non-empty (the constructor errors otherwise).
			attribute.String("domain", lz.domains[0]),
			attribute.StringSlice("domains", lz.domains),
		),
	)
	defer span.End()

	lz.smartTransportMu.Lock()

	if lz.smartTransport == nil {
		slog.Info("Smart transport is nil")
		trans, err := kindling.NewSmartHTTPTransportWithConfig(lz.logWriter, DialerConfig, bypass.StreamDialer(), nil, lz.domains...)
		if err != nil {
			slog.Info("Error creating smart transport", "error", err)
			lz.smartTransportMu.Unlock()
			return nil, traces.RecordError(ctx, fmt.Errorf("could not create smart transport -- offline? %v", err))
		}
		lz.smartTransport = trans
	}

	lz.smartTransportMu.Unlock()
	res, err := lz.smartTransport.RoundTrip(req.WithContext(ctx))
	if err != nil {
		traces.RecordError(ctx, err, trace.WithStackTrace(true))
	}
	return res, err
}
