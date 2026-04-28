// Package kindling provides a wrapper around the kindling library to create an HTTP client with
// various transports (domain fronting, AMP, DNS tunneling, proxyless) from a shared kindling instance.
package kindling

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/getlantern/kindling"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/common"
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
	// EnabledTransports gates which transports NewKindling wires up. Intended for tests; not a
	// production toggle.
	EnabledTransports = map[kindling.TransportName]bool{
		kindling.TransportDNSTunnel:   false,
		kindling.TransportAMP:         true,
		kindling.TransportSmart:       true,
		kindling.TransportDomainfront: true,
	}
	defaultTransportClone = http.DefaultTransport.(*http.Transport).Clone()

	transport http.RoundTripper
)

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

const tracerName = "github.com/getlantern/radiance/kindling"

// NewKindling builds a kindling client and registers package-level cleanup
// hooks; call [Close] to release them.
func NewKindling(dataDir string) (kindling.Kindling, error) {
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
		return kindling.NewKindling("radiance",
			kindling.WithPanicListener(reporting.PanicListener),
			kindling.WithLogWriter(logger),
			// "pro-server" calls still target api.getiantem.org; everything
			// else uses df.iantem.io.
			kindling.WithProxyless("df.iantem.io", "api.getiantem.org", "api.staging.iantem.io"),
		)
	}

	closeTransports = []func() error{}
	kindlingOptions := []kindling.Option{
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(logger),
	}

	updaterCtx, cancel := context.WithCancel(ctx)
	if enabled := EnabledTransports[kindling.TransportDomainfront]; enabled {
		f, err := fronted.NewFronted(updaterCtx, filepath.Join(dataDir, "fronted_cache.json"), logger)
		if err != nil {
			slog.Error("failed to create fronted client", slog.Any("error", err))
			span.RecordError(err)
		}
		if f != nil {
			closeTransports = append(closeTransports, func() error { f.Close(); return nil })
			kindlingOptions = append(kindlingOptions, kindling.WithDomainFronting(f))
		}
	}

	if enabled := EnabledTransports[kindling.TransportAMP]; enabled {
		ampClient, err := fronted.NewAMPClient(updaterCtx, dataDir, logger)
		if err != nil {
			slog.Error("failed to create amp client", slog.Any("error", err))
			span.RecordError(err)
		}
		if ampClient != nil {
			kindlingOptions = append(kindlingOptions, kindling.WithAMPCache(ampClient))
		}
	}

	if enabled := EnabledTransports[kindling.TransportDNSTunnel]; enabled {
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

	if enabled := EnabledTransports[kindling.TransportSmart]; enabled {
		// "pro-server" calls still target api.getiantem.org; everything
		// else uses df.iantem.io.
		kindlingOptions = append(kindlingOptions, kindling.WithProxyless("df.iantem.io", "api.getiantem.org"))
	}

	stopUpdater = cancel
	return kindling.NewKindling("radiance", kindlingOptions...)
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	s := strings.TrimSpace(string(p))
	w.Info(s)
	return len(p), nil
}

// SetKindling installs a test-supplied kindling instance and consumes
// initOnce, so it must be called before any Init or HTTPClient use or it
// will silently no-op.
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
