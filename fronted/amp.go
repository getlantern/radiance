package fronted

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/getlantern/amp"
)

// NewAMPClient creates a new AMP (Accelerated Mobile Pages) client for domain fronting.
// It initializes the client with the provided context, log writer, and public key for verification.
// The client automatically fetches and updates its configuration from a remote URL in the background.
// The context parameter controls the lifecycle of background configuration updates.
//   - ctx: Used to manage the lifecycle of background configuration updates.
//   - logWriter: Writer for logging transport and client activity.
//   - publicKey: Public key used to verify configuration signatures.
//
// Returns an initialized amp.Client or an error if setup fails.
func NewAMPClient(ctx context.Context, logWriter io.Writer, publicKey string) (amp.Client, error) {
	configURL := "https://raw.githubusercontent.com/getlantern/radiance/main/config/amp.yml.gz"
	httpClient, err := newHTTPClientWithSmartTRansport(logWriter, configURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart HTTP client: %w", err)
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
