package kindling

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling/dnstt"
	"github.com/getlantern/radiance/kindling/fronted"
	"github.com/getlantern/radiance/traces"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	k               kindling.Kindling
	kindlingMutex   sync.Mutex
	stopUpdater     func()
	closeTransports []func() error
)

// HTTPClient returns a http client with kindling transport
func HTTPClient() *http.Client {
	if k == nil {
		SetKindling(NewKindling())
	}
	httpClient := k.NewHTTPClient()
	httpClient.Timeout = common.DefaultHTTPTimeout
	httpClient.Transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(httpClient.Transport))
	return httpClient
}

// Close stop all concurrent config fetches that can be happening in background
func Close(_ context.Context) error {
	if stopUpdater != nil {
		stopUpdater()
	}
	for _, c := range closeTransports {
		if c != nil {
			if err := c(); err != nil {
				slog.Error("failed to close DNS tunnel", slog.Any("error", err))
			}
		}
	}
	return nil
}

// SetKindling sets the kindling method used for building the HTTP client
// This function is useful for testing purposes.
func SetKindling(a kindling.Kindling) {
	kindlingMutex.Lock()
	defer kindlingMutex.Unlock()
	k = a
}

const tracerName = "github.com/getlantern/radiance/kindling"

// NewKindling build a kindling client and bootstrap this package
func NewKindling() kindling.Kindling {
	dataDir := settings.GetString(settings.DataPathKey)
	logger := &slogWriter{Logger: slog.Default()}

	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"NewKindling",
		trace.WithAttributes(attribute.String("data_path", dataDir)),
	)
	defer span.End()

	updaterCtx, cancel := context.WithCancel(ctx)
	f, err := fronted.NewFronted(updaterCtx, reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), logger)
	if err != nil {
		slog.Error("failed to create fronted client", slog.Any("error", err))
		span.RecordError(err)
	}

	ampClient, err := fronted.NewAMPClient(updaterCtx, dataDir, logger)
	if err != nil {
		slog.Error("failed to create amp client", slog.Any("error", err))
		span.RecordError(err)
	}

	dnsttOptions, err := dnstt.DNSTTOptions(updaterCtx, filepath.Join(dataDir, "dnstt.yml.gz"), logger)
	if err != nil {
		slog.Error("failed to create or load dnstt kindling options", slog.Any("error", err))
		span.RecordError(err)
	}

	stopUpdater = cancel
	closeTransports = []func() error{
		func() error {
			if f != nil {
				f.Close()
			}
			return nil
		},
		func() error {
			if dnsttOptions != nil {
				dnsttOptions.Close()
			}
			return nil
		},
	}
	return kindling.NewKindling("radiance",
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(logger),
		// Most endpoints use df.iantem.io, but for some historical reasons
		// "pro-server" calls still go to api.getiantem.org.
		kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
		kindling.WithDomainFronting(f),
		// Kindling will skip amp transports if the request has a payload larger than 6kb
		kindling.WithAMPCache(ampClient),
		kindling.WithDNSTunnel(dnsttOptions),
	)
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	// Convert the byte slice to a string and log it
	w.Info(string(p))
	return len(p), nil
}
