// Package kindling provides a wrapper around the kindling library to create an HTTP client with
// various transports (domain fronting, AMP, DNS tunneling, proxyless) from a shared kindling instance.
package kindling

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/getlantern/kindling"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling/dnstt"
	"github.com/getlantern/radiance/kindling/fronted"
	radiancesmart "github.com/getlantern/radiance/kindling/smart"
	"github.com/getlantern/radiance/traces"
)

var (
	mu          sync.Mutex
	initialized bool
	k           *Client
	// EnabledTransports gates which transports NewKindling wires up. Intended for tests; not a
	// production toggle.
	EnabledTransports = map[kindling.TransportName]bool{
		kindling.TransportDNSTunnel:   true,
		kindling.TransportAMP:         true,
		kindling.TransportSmart:       true,
		kindling.TransportDomainfront: true,
	}
	defaultTransportClone = http.DefaultTransport.(*http.Transport).Clone()

	transport http.RoundTripper
)

// AMPEnabledForCountry reports whether the AMP transport should be wired up for
// the given country. AMP fronts through Google domains that are unreachable
// from China, so racing it there only wastes connection attempts.
func AMPEnabledForCountry(country string) bool {
	return !strings.EqualFold(country, "CN")
}

func initKindling() {
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

func Init() {
	go ensureInit()
}

func ensureInit() http.RoundTripper {
	mu.Lock()
	defer mu.Unlock()
	if !initialized {
		initKindling()
		initialized = true
	}
	return transport
}

// HTTPClient returns an HTTP client whose transport blocks on first use
// until kindling is initialized.
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout:   common.DNSTTHTTPTimeout,
		Transport: readyTransport{},
	}
}

type readyTransport struct{}

func (readyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return ensureInit().RoundTrip(req)
}

// Close stops any in-flight config fetches, releases kindling transports, and
// re-arms the package so the next Init or HTTPClient use rebuilds the stack.
func Close() error {
	// Mobile constructs a new LocalBackend in the same process after the system
	// extension stops, so Close must not be terminal.
	mu.Lock()
	defer mu.Unlock()
	if k != nil {
		if err := k.Close(); err != nil {
			slog.Error("failed to close kindling transports", slog.Any("error", err))
		}
		k = nil
	}
	transport = nil
	initialized = false
	return nil
}

const tracerName = "github.com/getlantern/radiance/kindling"

// Client is a kindling instance together with the transport resources its
// construction created (config updaters, fronted/dnstt state).
type Client struct {
	kindling.Kindling
	cancel    context.CancelFunc
	closers   []func() error
	closeOnce sync.Once
}

// Close cancels the transports' config updaters and releases their resources.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		var errs []error
		for _, cl := range c.closers {
			errs = append(errs, cl())
		}
		err = errors.Join(errs...)
	})
	return err
}

// NewKindling builds a kindling client. On error, any partially built
// transport state is released before returning.
func NewKindling(dataDir string) (*Client, error) {
	logger := &slogWriter{Logger: slog.Default()}

	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"NewKindling",
		trace.WithAttributes(attribute.String("data_path", dataDir)),
	)
	defer span.End()

	if common.Stage() {
		// Staging runs proxyless-only; fronted client failures against staging
		// hosts otherwise obscure the backend behavior we want to test.
		newK, err := kindling.NewKindling("radiance",
			kindling.WithPanicListener(reporting.PanicListener),
			kindling.WithLogWriter(logger),
			kindling.WithStreamDialer(bypass.StreamDialer()),
			kindling.WithSmartDialerConfig(radiancesmart.DialerConfig),
			// "pro-server" calls still target api.getiantem.org; everything
			// else uses df.iantem.io.
			kindling.WithProxyless("df.iantem.io", "api.getiantem.org", "api.staging.iantem.io"),
		)
		if err != nil {
			return nil, err
		}
		return &Client{Kindling: newK}, nil
	}

	var closers []func() error
	kindlingOptions := []kindling.Option{
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(logger),
		kindling.WithStreamDialer(bypass.StreamDialer()),
		kindling.WithSmartDialerConfig(radiancesmart.DialerConfig),
	}

	updaterCtx, cancel := context.WithCancel(ctx)
	if enabled := EnabledTransports[kindling.TransportDomainfront]; enabled {
		f, err := fronted.NewFronted(updaterCtx, filepath.Join(dataDir, "fronted_cache.json"), logger)
		if err != nil {
			slog.Error("failed to create fronted client", slog.Any("error", err))
			span.RecordError(err)
		}
		if f != nil {
			closers = append(closers, func() error { f.Close(); return nil })
			kindlingOptions = append(kindlingOptions, kindling.WithDomainFronting(f))
		}
	}

	// env.Country overrides the config-derived country and isn't always mirrored
	// into settings (e.g. RADIANCE_COUNTRY set at launch), so resolve it here.
	country := settings.GetString(settings.CountryCodeKey)
	if override := env.GetString(env.Country); override != "" {
		country = override
	}
	if EnabledTransports[kindling.TransportAMP] && AMPEnabledForCountry(country) {
		ampClient, err := fronted.NewAMPClient(updaterCtx, dataDir, logger)
		if err != nil {
			slog.Error("failed to create amp client", slog.Any("error", err))
			span.RecordError(err)
		}
		if ampClient != nil {
			kindlingOptions = append(kindlingOptions, kindling.WithAMPCache(ampClient))
		}
	}

	if enabled := EnabledTransports[kindling.TransportSmart]; enabled {
		// "pro-server" calls still target api.getiantem.org; everything
		// else uses df.iantem.io.
		kindlingOptions = append(kindlingOptions, kindling.WithProxyless("df.iantem.io", "api.getiantem.org"))
	}

	if enabled := EnabledTransports[kindling.TransportDNSTunnel]; enabled {
		dnsttOptions, err := dnstt.DNSTTOptions(updaterCtx, filepath.Join(dataDir, "dnstt.yml.gz"), logger)
		if err != nil {
			slog.Error("failed to create or load dnstt kindling options", slog.Any("error", err))
			span.RecordError(err)
		}
		if dnsttOptions != nil {
			closers = append(closers, dnsttOptions.Close)
			kindlingOptions = append(kindlingOptions, kindling.WithDNSTunnel(dnsttOptions))
		}
	}

	newK, err := kindling.NewKindling("radiance", kindlingOptions...)
	if err != nil {
		errs := []error{err}
		cancel()
		for _, cl := range closers {
			errs = append(errs, cl())
		}
		return nil, errors.Join(errs...)
	}
	return &Client{Kindling: newK, cancel: cancel, closers: closers}, nil
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	s := strings.TrimSpace(string(p))
	w.Info(s)
	return len(p), nil
}

// SetKindling installs a test-supplied kindling instance and claims
// initialization — call it before Init or the first request through
// HTTPClient, or it will silently no-op. The package Close closes the
// installed instance.
func SetKindling(c *Client) {
	mu.Lock()
	defer mu.Unlock()
	if initialized {
		return
	}
	k = c
	if c != nil {
		transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(c.NewHTTPClient().Transport))
	} else {
		transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(defaultTransportClone))
	}
	initialized = true
}
