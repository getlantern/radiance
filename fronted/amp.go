package fronted

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/getlantern/amp"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/traces"
)

func NewAMPClient(ctx context.Context, logWriter io.Writer, publicKey string) (amp.Client, error) {
	configURL := "https://raw.githubusercontent.com/getlantern/radiance/main/config/amp.yml.gz"
	u, err := url.Parse(configURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}
	domain := u.Host
	trans, err := kindling.NewSmartHTTPTransport(logWriter, domain)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart HTTP transport: %v", err)
	}
	lz := &lazyDialingRoundTripper{
		smartTransportMu: sync.Mutex{},
		logWriter:        logWriter,
		domain:           domain,
	}
	if trans != nil {
		lz.smartTransport = trans
	}

	httpClient := &http.Client{
		Transport: traces.NewRoundTripper(lz),
	}

	ampClient, err := amp.NewClientWithConfig(ctx,
		amp.Config{
			BrokerURL: "https://amp.iantem.io",
			CacheURL:  "https://cdn.ampproject.org",
			Fronts:    []string{"google.com", "youtube.com", "photos.google.com"},
			PublicKey: publicKey,
		},
		amp.WithConfigURL(configURL),
		amp.WithHTTPClient(httpClient),
		amp.WithPollInterval(12*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("failed to build amp client: %w", err)
	}
	return ampClient, nil
}
