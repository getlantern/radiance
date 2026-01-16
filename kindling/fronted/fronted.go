package fronted

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/getlantern/fronted"
	"github.com/getlantern/radiance/kindling/smart"
)

const tracerName = "github.com/getlantern/radiance/fronted"

func NewFronted(panicListener func(string), cacheFile string, logWriter io.Writer) (fronted.Fronted, error) {
	configURL := "https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz"
	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	httpClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, configURL)
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
