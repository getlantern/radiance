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
	"github.com/getlantern/radiance/kindling/fronted"
	"github.com/getlantern/radiance/traces"
)

var (
	k             kindling.Kindling
	kindlingMutex sync.Mutex
	stopUpdater   func()
)

// HTTPClient returns a http client with kindling transport
func HTTPClient() *http.Client {
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
	return nil
}

// SetKindling sets the kindling method used for building the HTTP client
// This function is useful for testing purposes.
func SetKindling(a kindling.Kindling) {
	kindlingMutex.Lock()
	defer kindlingMutex.Unlock()
	k = a
}

// NewKindling build a kindling client and bootstrap this package
func NewKindling() kindling.Kindling {
	dataDir := settings.GetString(settings.DataPathKey)
	logger := &slogWriter{Logger: slog.Default()}
	updaterCtx, cancel := context.WithCancel(context.Background())

	f, err := fronted.NewFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), logger)
	if err != nil {
		slog.Error("failed to create fronted client", slog.Any("error", err))
	}

	ampClient, err := fronted.NewAMPClient(updaterCtx, dataDir, logger)
	if err != nil {
		slog.Error("failed to create amp client", slog.Any("error", err))
	}

	stopUpdater = cancel
	return kindling.NewKindling("radiance",
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(logger),
		// Most endpoints use df.iantem.io, but for some historical reasons
		// "pro-server" calls still go to api.getiantem.org.
		kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
		kindling.WithDomainFronting(f),
		// Kindling will skip amp transports if the request has a payload larger than 6kb
		kindling.WithAMPCache(ampClient),
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
