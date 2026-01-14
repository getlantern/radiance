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
	k             kindling.Kindling = newKindling()
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

func Close() {
	if stopUpdater != nil {
		stopUpdater()
	}
}

// SetKindling sets the kindling method used for building the HTTP client
// This function is useful for testing purposes.
func SetKindling(a kindling.Kindling) {
	kindlingMutex.Lock()
	defer kindlingMutex.Unlock()
	k = a
}

// newKindling build a kindling client and bootstrap this package
func newKindling() kindling.Kindling {
	dataDir := settings.GetString(settings.DataPathKey)
	logger := &slogWriter{Logger: slog.Default()}
	updaterCtx, cancel := context.WithCancel(context.Background())

	kindlingOptions := []kindling.Option{
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(logger),
		// Most endpoints use df.iantem.io, but for some historical reasons
		// "pro-server" calls still go to api.getiantem.org.
		kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
	}

	f, err := fronted.NewFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), logger)
	if err != nil {
		slog.Error("failed to create fronted client", slog.Any("error", err))
	} else {
		kindlingOptions = append(kindlingOptions, kindling.WithDomainFronting(f))
	}

	ampClient, err := fronted.NewAMPClient(updaterCtx, dataDir, logger)
	if err != nil {
		slog.Error("failed to create amp client", slog.Any("error", err))
	} else {
		// Kindling will skip amp transports if the request has a payload larger than 6kb
		kindlingOptions = append(kindlingOptions, kindling.WithAMPCache(ampClient))
	}

	kindlingMutex.Lock()
	defer kindlingMutex.Unlock()
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
