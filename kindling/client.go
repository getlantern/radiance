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
	// EnabledTransports is used for testing purposes for enabling/disabling kindling transports
	EnabledTransports = map[string]bool{
		"dnstt":     false,
		"amp":       true,
		"proxyless": true,
		"fronted":   true,
	}
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
				slog.Error("failed to close kindling transport", slog.Any("error", err))
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
		closeTransports = append(closeTransports, func() error {
			if f != nil {
				f.Close()
			}
			return nil
		})
		kindlingOptions = append(kindlingOptions, kindling.WithDomainFronting(f))
	}

	if enabled := EnabledTransports["amp"]; enabled {
		ampClient, err := fronted.NewAMPClient(updaterCtx, dataDir, logger)
		if err != nil {
			slog.Error("failed to create amp client", slog.Any("error", err))
			span.RecordError(err)
		}
		// Kindling will skip amp transports if the request has a payload larger than 6kb
		kindlingOptions = append(kindlingOptions, kindling.WithAMPCache(ampClient))
	}

	if enabled := EnabledTransports["dnstt"]; enabled {
		dnsttOptions, err := dnstt.DNSTTOptions(updaterCtx, filepath.Join(dataDir, "dnstt.yml.gz"), logger)
		if err != nil {
			slog.Error("failed to create or load dnstt kindling options", slog.Any("error", err))
			span.RecordError(err)
		}
		if dnsttOptions != nil {
			closeTransports = append(closeTransports, dnsttOptions.Close)
		}
		kindlingOptions = append(kindlingOptions, kindling.WithDNSTunnel(dnsttOptions))
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
	w.Info(string(p))
	return len(p), nil
}
