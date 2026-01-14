package kindling

import (
	"context"
	"fmt"
	"io"
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
	k kindling.Kindling
	// defaultOptions generally does not change after the first time
	// or if they change, it's handled internally
	defaultOptions = make([]kindling.Option, 0)
	mutexOptions   sync.Mutex
)

// HTTPClient returns a http client with kindling transport
func HTTPClient() *http.Client {
	mutexOptions.Lock()
	defer mutexOptions.Unlock()

	if k == nil {
		mutexOptions.Unlock()
		err := NewKindling(context.Background(), settings.GetString(settings.DataPathKey), &slogWriter{Logger: slog.Default()})
		if err != nil {
			slog.Error("failed to build kindling", slog.Any("error", err))
			return &http.Client{}
		}
		mutexOptions.Lock()
	}

	httpClient := k.NewHTTPClient()
	httpClient.Timeout = common.DefaultHTTPTimeout
	httpClient.Transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(httpClient.Transport))
	return httpClient
}

// SetKindling sets the kindling method used for building the HTTP client
// This function is useful for testing purposes.
func SetKindling(a kindling.Kindling) {
	mutexOptions.Lock()
	defer mutexOptions.Unlock()
	k = a
}

// NewKindling build a kindling client and bootstrap this package
func NewKindling(ctx context.Context, dataDir string, logger io.Writer) error {
	mutexOptions.Lock()
	defer mutexOptions.Unlock()

	if len(defaultOptions) == 0 {
		defaultOptions = append(defaultOptions,
			kindling.WithPanicListener(reporting.PanicListener),
			kindling.WithLogWriter(logger),
			// Most endpoints use df.iantem.io, but for some historical reasons
			// "pro-server" calls still go to api.getiantem.org.
			kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
		)

		f, err := fronted.NewFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), logger)
		if err != nil {
			return fmt.Errorf("failed to create fronted: %w", err)
		} else {
			defaultOptions = append(defaultOptions, kindling.WithDomainFronting(f))
		}

		ampClient, err := fronted.NewAMPClient(ctx, dataDir, logger)
		if err != nil {
			return fmt.Errorf("failed to create amp client: %w", err)
		} else {
			// Kindling will skip amp transports if the request has a payload larger than 6kb
			defaultOptions = append(defaultOptions, kindling.WithAMPCache(ampClient))
		}
	}

	k = kindling.NewKindling("radiance", defaultOptions...)
	return nil
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	// Convert the byte slice to a string and log it
	w.Info(string(p))
	return len(p), nil
}
