package fronted

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
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
			PublicKey: publicKey,
			Fronts: []string{
				"google.com",
				"youtube.com",
				"photos.google.com",
				"gmail.com",
			},
		},
		amp.WithConfigURL(configURL),
		amp.WithHTTPClient(httpClient),
		amp.WithPollInterval(12*time.Hour),
		amp.WithDialer(func(network, address string) (net.Conn, error) {
			serverName, _, semicolonExists := strings.Cut(address, ":")
			addressWithPort := address
			// if address doesn't contain a port, by default use :443
			if !semicolonExists {
				addressWithPort = fmt.Sprintf("%s:443", serverName)
			}
			return tls.Dial("tcp", addressWithPort, &tls.Config{
				ServerName: serverName,
			})
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build amp client: %w", err)
	}
	return ampClient, nil
}
