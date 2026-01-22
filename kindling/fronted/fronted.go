package fronted

import (
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
	frontedOptions := []fronted.Option{
		fronted.WithPanicListener(panicListener),
		fronted.WithCacheFile(cacheFile),
	}
	httpClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, configURL)
	if err != nil {
		slog.Error("failed to build http client with smart HTTP transport", slog.Any("error", err))
	} else {
		frontedOptions = append(frontedOptions, fronted.WithHTTPClient(httpClient), fronted.WithConfigURL(configURL))
	}

	fronted.SetLogger(slog.Default())
	return fronted.NewFronted(frontedOptions...), nil
}
