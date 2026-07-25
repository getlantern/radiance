// Package smart contains a HTTP client with smart transport used by other
// methods to fetch config updates
package smart

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

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

// newSmartTransport is a test seam: a real strategy search needs network
// reachability that tests don't have.
var newSmartTransport = func(logWriter io.Writer, host string) (http.RoundTripper, error) {
	trans, err := kindling.NewSmartHTTPTransportWithConfig(logWriter, DialerConfig, bypass.StreamDialer(), nil, host)
	if err != nil {
		return nil, err
	}
	if trans == nil {
		return nil, fmt.Errorf("no smart transport for %q", host)
	}
	return trans, nil
}

// NewHTTPClientWithSmartTransport returns an HTTP client that reaches each
// config URL's host through its own Outline smart dialer. At least one URL with
// a host is required.
//
// One dialer per host rather than one dialer covering all of them: the strategy
// search is conjunctive, so a single dialer given several hosts succeeds only if
// one strategy unblocks every one of them, and a host that is blocked outright
// then takes the reachable ones down with it.
func NewHTTPClientWithSmartTransport(logWriter io.Writer, addresses ...string) (*http.Client, error) {
	hosts, err := configHosts(addresses)
	if err != nil {
		return nil, err
	}
	byHost := make(map[string]*hostTransport, len(hosts))
	for _, host := range hosts {
		ht := &hostTransport{
			host:         host,
			logWriter:    logWriter,
			newTransport: newSmartTransport,
			searching:    make(chan struct{}, 1),
		}
		byHost[host] = ht
		// Searching in the background keeps the client constructible while
		// offline yet still spends the search before anything waits on it.
		// Callers bound a whole config fetch at a timeout of the same order as
		// the search itself, so one that pays for the search inside that budget
		// has little of it left for the request.
		go func() { _, _ = ht.roundTripper(context.Background()) }()
	}
	lz := &lazyDialingRoundTripper{hosts: hosts, byHost: byHost}
	return &http.Client{Transport: traces.NewRoundTripper(lz)}, nil
}

// configHosts returns each address's host, in order and deduplicated.
func configHosts(addresses []string) ([]string, error) {
	seen := make(map[string]bool, len(addresses))
	hosts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		u, err := url.Parse(address)
		if err != nil {
			return nil, fmt.Errorf("failed to parse URL %q: %w", address, err)
		}
		// Hostname() drops any :port; skip empties (e.g. scheme-less URLs) and
		// dedup so a shared host isn't dialed through two dialers.
		host := u.Hostname()
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no valid config host among %v", addresses)
	}
	return hosts, nil
}

// lazyDialingRoundTripper routes each request to its own host's transport, so a
// host that never becomes reachable costs only the requests aimed at it.
//
// byHost is written only during construction, so reads need no lock; each entry
// guards its own transport.
type lazyDialingRoundTripper struct {
	hosts  []string
	byHost map[string]*hostTransport
}

var _ http.RoundTripper = (*lazyDialingRoundTripper)(nil)

func (lz *lazyDialingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ht := lz.transportFor(req.URL.Hostname())
	ctx, span := otel.Tracer(tracerName).Start(
		req.Context(),
		"lazy_dialing_round_trip",
		trace.WithAttributes(
			attribute.String("domain", ht.host),
			attribute.StringSlice("domains", lz.hosts),
		),
	)
	defer span.End()

	trans, err := ht.roundTripper(ctx)
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("could not create smart transport for %q -- offline? %w", ht.host, err))
	}
	res, err := trans.RoundTrip(req.WithContext(ctx))
	if err != nil {
		traces.RecordError(ctx, err, trace.WithStackTrace(true))
	}
	return res, err
}

// transportFor returns host's transport, falling back to the first configured
// host's: one tuned for a sibling config source is a better guess than failing
// the request outright.
func (lz *lazyDialingRoundTripper) transportFor(host string) *hostTransport {
	if ht, ok := lz.byHost[host]; ok {
		return ht
	}
	return lz.byHost[lz.hosts[0]]
}

type hostTransport struct {
	host      string
	logWriter io.Writer

	// newTransport is captured at construction rather than read from the
	// package var, so that a search still running in the background can't race
	// a test restoring the seam.
	newTransport func(logWriter io.Writer, host string) (http.RoundTripper, error)

	// searching admits one strategy search at a time, and is a channel rather
	// than a mutex so that a caller can abandon its wait when its own context
	// ends while the search runs on for whoever comes next.
	searching chan struct{}
	trans     http.RoundTripper
}

// roundTripper returns the transport for this host, running the strategy search
// on first use — so it blocks for however long that search takes, or until ctx
// ends. A failed search is deliberately not remembered: this requires callers
// to retry on their own schedule, in exchange for letting a host blocked at
// first contact recover without a restart.
func (ht *hostTransport) roundTripper(ctx context.Context) (http.RoundTripper, error) {
	select {
	case ht.searching <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-ht.searching }()

	if ht.trans != nil {
		return ht.trans, nil
	}
	trans, err := ht.newTransport(ht.logWriter, ht.host)
	if err != nil {
		slog.Warn("smart dialer found no working strategy", "host", ht.host, "error", err)
		return nil, err
	}
	ht.trans = trans
	return trans, nil
}
