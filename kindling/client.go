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
	defaultTransportClone = http.DefaultTransport.(*http.Transport).Clone()
)

// HTTPClient returns an HTTP client whose transport lazily initializes
// kindling on the first request. This avoids blocking startup while still
// providing censorship-circumvention transports once they are needed.
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout:   common.DefaultHTTPTimeout,
		Transport: &lazyTransport{},
	}
}

// lazyTransport is an http.RoundTripper that initializes the kindling-backed
// transport on the first RoundTrip call.
type lazyTransport struct {
	once sync.Once
	rt   http.RoundTripper
}

func (t *lazyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.once.Do(func() {
		kindlingMutex.Lock()
		if k == nil {
			newK, err := NewKindling(settings.GetString(settings.DataPathKey))
			if err != nil {
				slog.Error("failed to create kindling client", slog.Any("error", err))
			}
			if newK != nil {
				k = newK
			}
		}
		localK := k
		kindlingMutex.Unlock()

		if localK != nil {
			t.rt = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(localK.NewHTTPClient().Transport))
		} else {
			slog.Warn("kindling unavailable, using default transport clone")
			t.rt = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(defaultTransportClone))
		}
	})
	return t.rt.RoundTrip(req)
}

// Close stop all concurrent config fetches that can be happening in background
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

// SetKindling sets the kindling method used for building the HTTP client
// This function is useful for testing purposes.
func SetKindling(a kindling.Kindling) {
	kindlingMutex.Lock()
	defer kindlingMutex.Unlock()
	k = a
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
