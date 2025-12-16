//go:build integration
// +build integration

package radiance

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/getlantern/dnstt"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/fronted"
	"github.com/getlantern/radiance/issue"
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
		) kindling.Kindling
		assert func(t *testing.T, err error)
	}

	email := ""
	userID := ""

	tests := []testCase{
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
			) kindling.Kindling {
				f, err := fronted.NewFronted(
					reporting.PanicListener,
					filepath.Join(dataDir, "fronted_cache.json"),
					logger,
				)
				require.NoError(t, err)

				return kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithDomainFronting(f),
				)
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name:        "Proxyless Kindling",
			description: "proxyless testing",
			userID:      userID,
			locale:      "pt-BR",
			email:       email,
			country:     "BR",
			setup: func(
				_ context.Context,
				t *testing.T,
				dataDir string,
				logger *slogWriter,
			) kindling.Kindling {
				return kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
				)
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name:        "DNSTT Kindling",
			description: "dnstt testing",
			userID:      userID,
			locale:      "pt-BR",
			email:       email,
			country:     "BR",
			setup: func(
				_ context.Context,
				t *testing.T,
				dataDir string,
				logger *slogWriter,
			) kindling.Kindling {
				cli, err := dnstt.NewDNSTT(
					dnstt.WithDoH("https://cloudflare-dns.com/dns-query"),
					dnstt.WithTunnelDomain("t.iantem.io"),
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
				dnsTunnel4, err := dnstt.NewDNSTT(
					dnstt.WithTunnelDomain("t.iantem.io"),
					dnstt.WithDoT("dns.quad9.net:853"),
					dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
				)
				require.NoError(t, err)

				return kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithDNSTunnel(cli),
					kindling.WithDNSTunnel(dnsTunnel2),
					kindling.WithDNSTunnel(dnsTunnel3),
					kindling.WithDNSTunnel(dnsTunnel4),
				)
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name:        "AMP Kindling",
			description: "amp testing",
			userID:      userID,
			locale:      "en-US",
			email:       email,
			country:     "BR",
			setup: func(
				ctx context.Context,
				t *testing.T,
				dataDir string,
				logger *slogWriter,
			) kindling.Kindling {
				ampClient, err := fronted.NewAMPClient(ctx, logger, ampPublicKey)
				require.NoError(t, err)

				return kindling.NewKindling(
					"radiance",
					kindling.WithLogWriter(logger),
					kindling.WithAMPCache(ampClient),
				)
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			logDir := t.TempDir()
			require.NoError(t, common.Init(dataDir, logDir, "TRACE"))
			kindlingLogger := &slogWriter{Logger: slog.Default()}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			k := tc.setup(ctx, t, dataDir, kindlingLogger)

			httpClientWithTimeout := k.NewHTTPClient()
			httpClientWithTimeout.Timeout = common.DefaultHTTPTimeout

			reporter, err := issue.NewIssueReporter(
				httpClientWithTimeout,
				common.NewUserConfig(tc.userID, dataDir, tc.locale),
			)
			require.NoError(t, err)

			t.Run("reporting issue should work", func(t *testing.T) {
				//  ~15MB payload
				// const size = 1 * 1000000
				// Base64 inflates: 3 bytes → 4 chars
				// raw := make([]byte, size*3/4+3) // +3 to avoid truncation issues

				// _, err = rand.Read(raw)
				// require.NoError(t, err)

				// s := base64.RawURLEncoding.EncodeToString(raw)
				// s = s[:size] // exact length
				assert.NoError(t, reporter.Report(
					context.Background(),
					issue.IssueReport{
						Type:        "Other",
						Description: tc.description,
						Device:      "test",
						Model:       "test",
						Attachments: []*issue.Attachment{
							&issue.Attachment{
								Name: "Hello.txt",
								Data: []byte("hello world"),
							},
						},
					},
					tc.email,
					tc.country,
				))
			})
		})
	}
}
