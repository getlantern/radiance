package fronted

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"

	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/traces"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/getlantern/radiance/fronted"

func NewFronted(panicListener func(string), cacheFile string, logWriter io.Writer) (fronted.Fronted, error) {
	configURL := "https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz"
	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	httpClient, err := newHTTPClientWithSmartTransport(logWriter, configURL)
	if err != nil {
		return nil, fmt.Errorf("failed to build http client with smart HTTP transport: %w", err)
	}

	fronted.SetLogger(slog.Default())
	return fronted.NewFronted(
		fronted.WithPanicListener(panicListener),
		fronted.WithCacheFile(cacheFile),
		fronted.WithHTTPClient(httpClient),
		fronted.WithConfigURL(configURL),
	), nil
}

func newHTTPClientWithSmartTransport(logWriter io.Writer, address string) (*http.Client, error) {
	// Parse the domain from the URL.
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}

	// Extract the domain from the URL.
	domain := u.Host
	trans, err := kindling.NewSmartHTTPTransport(logWriter, domain)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart HTTP transport: %v", err)
	}
	lz := &lazyDialingRoundTripper{
		smartTransportMu: sync.Mutex{},
		logWriter:        logWriter,
		domain:           domain}
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
	domain    string
}

// Make sure lazyDialingRoundTripper implements http.RoundTripper
var _ http.RoundTripper = (*lazyDialingRoundTripper)(nil)

func (lz *lazyDialingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer(tracerName).Start(req.Context(), "lazy_dialing_round_trip")
	defer span.End()

	lz.smartTransportMu.Lock()

	if lz.smartTransport == nil {
		slog.Info("Smart transport is nil")
		var err error
		lz.smartTransport, err = kindling.NewSmartHTTPTransport(lz.logWriter, lz.domain)
		if err != nil {
			slog.Info("Error creating smart transport", "error", err)
			lz.smartTransportMu.Unlock()
			// This typically just means we're offline
			return nil, traces.RecordError(ctx, fmt.Errorf("could not create smart transport -- offline? %v", err))
		}
	}

	lz.smartTransportMu.Unlock()
	res, err := lz.smartTransport.RoundTrip(req.WithContext(ctx))
	if err != nil {
		traces.RecordError(ctx, err, trace.WithStackTrace(true))
	}
	return res, err
}
