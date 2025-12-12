//go:build integration
// +build integration

package radiance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getlantern/dnstt"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/fronted"
	"github.com/getlantern/radiance/issue"
	"github.com/getlantern/radiance/traces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKindlingIntegrations try to test each one of the techniques used by kindling
// for reporting issues and also trying to reach normal services.
// Please replace userID and email fields
// You can run this test by executing: `go test -tags integration -run TestKindlingIntegrations ./...`
func TestKindlingIntegrations(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	type testCase struct {
		name        string
		description string
		userID      string
		locale      string
		email       string
		country     string
		setup       func(
			ctx context.Context,
			t *testing.T,
			dataDir string,
			logger *slogWriter,
		) (kindling.Kindling, func(k kindling.Kindling, client *http.Client))
	}

	email := ""
	userID := ""

	tests := []testCase{
		{
			name:        "DNSTT Kindling",
			description: "dnstt testing",
			userID:      userID,
			locale:      "en-US",
			email:       email,
			country:     "US",
			setup: func(
				ctx context.Context,
				t *testing.T,
				dataDir string,
				logger *slogWriter,
			) (kindling.Kindling, func(k kindling.Kindling, client *http.Client)) {
				dnsTunnel, err := dnstt.NewDNSTT(
					dnstt.WithTunnelDomain("t.iantem.io"),
					dnstt.WithDoH("https://cloudflare-dns.com/dns-query"),
					dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
				)
				require.NoError(t, err)

				dnsTunnel2, err := dnstt.NewDNSTT(
					dnstt.WithTunnelDomain("t.iantem.io"),
					dnstt.WithDoH("https://dns.adguard-dns.com/dns-query"),
					dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
				)
				require.NoError(t, err)

				dnsTunnel3, err := dnstt.NewDNSTT(
					dnstt.WithTunnelDomain("t.iantem.io"),
					dnstt.WithDoH("https://dns.google/dns-query"),
					dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
				)
				require.NoError(t, err)

				k := kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithDNSTunnel(dnsTunnel),
					kindling.WithDNSTunnel(dnsTunnel2),
					kindling.WithDNSTunnel(dnsTunnel3),
				)

				after := func(k kindling.Kindling, client *http.Client) {
					events.Subscribe(func(e config.NewDNSTTConfigEvent) {
						slog.Info("updating kindling with latest dnstt config")
						// replacing dnstt roundtripper and making http client replace transports
						k.ReplaceRoundTripGenerator("dnstt", e.New.NewRoundTripper)
						// re-create race transport
						k.NewHTTPClient()
						// add trace roundtripper again
						client.Transport = traces.NewRoundTripper(
							traces.NewHeaderAnnotatingRoundTripper(client.Transport),
						)
					})
					config.DNSTTConfigUpdate(
						ctx,
						"https://raw.githubusercontent.com/getlantern/radiance/main/config/dnstt.yml.gz",
						client,
						12*time.Hour,
					)
					time.Sleep(5 * time.Second)
				}

				return k, after
			},
		},
		{
			name:        "AMP Kindling",
			description: "amp testing",
			userID:      userID,
			locale:      "en-US",
			email:       email,
			country:     "US",
			setup: func(
				ctx context.Context,
				t *testing.T,
				dataDir string,
				logger *slogWriter,
			) (kindling.Kindling, func(k kindling.Kindling, client *http.Client)) {
				ampClient, err := fronted.NewAMPClient(ctx, logger, ampPublicKey)
				require.NoError(t, err)

				k := kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithAMPCache(ampClient),
				)

				return k, nil
			},
		},
		{
			name:        "Fronted Kindling",
			description: "fronted testing",
			userID:      userID,
			locale:      "pt-BR",
			email:       email,
			country:     "BR",
			setup: func(
				_ context.Context,
				t *testing.T,
				dataDir string,
				logger *slogWriter,
			) (kindling.Kindling, func(k kindling.Kindling, client *http.Client)) {
				f, err := fronted.NewFronted(
					reporting.PanicListener,
					filepath.Join(dataDir, "fronted_cache.json"),
					logger,
				)
				require.NoError(t, err)

				k := kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithDomainFronting(f),
				)

				return k, nil
			},
		},
		{
			name:        "Proxyless Kindling",
			description: "proxyless testing",
			userID:      userID,
			locale:      "en-US",
			email:       email,
			country:     "US",
			setup: func(
				_ context.Context,
				_ *testing.T,
				_ string,
				logger *slogWriter,
			) (kindling.Kindling, func(k kindling.Kindling, client *http.Client)) {
				k := kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithProxyless("df.iantem.io", "detectportal.firefox.com"),
				)

				return k, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			kindlingLogger := &slogWriter{Logger: slog.Default()}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			k, after := tc.setup(ctx, t, dataDir, kindlingLogger)

			httpClientWithTimeout := k.NewHTTPClient()
			httpClientWithTimeout.Timeout = 1 * time.Minute

			if after != nil {
				after(k, httpClientWithTimeout)
			}

			reporter, err := issue.NewIssueReporter(
				httpClientWithTimeout,
				common.NewUserConfig(tc.userID, dataDir, tc.locale),
			)
			require.NoError(t, err)

			t.Run("reporting issue should work", func(t *testing.T) {
				// ~15MB payload
				const size = 15 * 1000000
				// Base64 inflates: 3 bytes → 4 chars
				raw := make([]byte, size*3/4+3) // +3 to avoid truncation issues

				_, err = rand.Read(raw)
				require.NoError(t, err)

				s := base64.RawURLEncoding.EncodeToString(raw)
				s = s[:size] // exact length
				assert.NoError(t, reporter.Report(
					context.Background(),
					issue.IssueReport{
						Type:        "Other",
						Description: tc.description,
						Device:      "test",
						Model:       "test",
						Attachments: []*issue.Attachment{
							{
								Name: "Hello.txt",
								Data: []byte(s),
							},
						},
					},
					tc.email,
					tc.country,
				))
			})

			t.Run("sending request to detectportal should succeed", func(t *testing.T) {
				req, err := http.NewRequest(
					http.MethodPost,
					"https://detectportal.firefox.com/success.txt",
					http.NoBody,
				)
				require.NoError(t, err)

				response, err := httpClientWithTimeout.Do(req)
				require.NoError(t, err)
				defer response.Body.Close()

				assert.Equal(t, http.StatusOK, response.StatusCode)

				content, err := io.ReadAll(response.Body)
				require.NoError(t, err)

				t.Log(response.StatusCode)
				t.Log(string(content))
			})
		})
	}
}
