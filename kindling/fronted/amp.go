package fronted

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	_ "embed"

	"github.com/getlantern/amp"
	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/kindling/smart"
	"github.com/getlantern/radiance/traces"
	"go.opentelemetry.io/otel"
)

//go:embed amp_public_key.pem
var ampPublicKey string

const ampConfigURL = "https://raw.githubusercontent.com/getlantern/radiance/main/kindling/fronted/amp.yml.gz"

// NewAMPClient creates a new AMP (Accelerated Mobile Pages) client for domain fronting.
// It initializes the client with the provided context, log writer, and public key for verification.
// The client automatically fetches and updates its configuration from a remote URL in the background.
// The context parameter controls the lifecycle of background configuration updates.
//   - ctx: Used to manage the lifecycle of background configuration updates.
//   - logWriter: Writer for logging transport and client activity.
//
// Returns an initialized amp.Client or an error if setup fails.
func NewAMPClient(ctx context.Context, storagePath string, logWriter io.Writer) (amp.Client, error) {
	_, span := otel.Tracer(tracerName).Start(
		ctx,
		"NewAMPClient",
	)
	defer span.End()
	ampOptions := []amp.Option{
		amp.WithConfig(amp.Config{
			BrokerURL: "https://amp.iantem.io",
			CacheURL:  "https://cdn.ampproject.org",
			PublicKey: ampPublicKey,
			Fronts: []string{
				"google.com",
				"developers.google.com",
				"docs.google.com",
				"drive.google.com",
				"console.firebase.google.com",
				"appengine.google.com",
				"compute.googleapis.com",
				"run.googleapis.com",
				"cloudfunctions.googleapis.com",
				"container.googleapis.com",
				"pubsub.googleapis.com",
				"fonts.googleapis.com",
				"fonts.gstatic.com",
				"blogspot.com",
				"play.google.com",
				"developers.google.cn",
			},
		}),
		amp.WithConfigStoragePath(storagePath),
		amp.WithDialer(func(network, address string) (net.Conn, error) {
			serverName, _, hasSemicolon := strings.Cut(address, ":")
			addrWithPort := address
			if !hasSemicolon {
				addrWithPort = fmt.Sprintf("%s:443", serverName)
			}
			conn, err := bypass.Dial("tcp", addrWithPort)
			if err != nil {
				return nil, err
			}
			tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName})
			if err := tlsConn.HandshakeContext(context.Background()); err != nil {
				conn.Close()
				return nil, err
			}
			return tlsConn, nil
		}),
	}
	httpClient, err := smart.NewHTTPClientWithSmartTransport(logWriter, ampConfigURL)
	if err != nil {
		span.RecordError(err)
		slog.Error("failed to create smart HTTP client", slog.Any("error", err))
	} else {
		ampOptions = append(ampOptions,
			amp.WithHTTPClient(httpClient),
			amp.WithPollInterval(12*time.Hour),
			amp.WithConfigURL(ampConfigURL))
	}

	ampClient, err := amp.NewClientWithOptions(ctx, ampOptions...)
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to build amp client: %w", err))
	}
	return ampClient, nil
}
