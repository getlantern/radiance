//go:build integration
// +build integration

package radiance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

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
		) (kindling.Kindling, func(k kindling.Kindling, client *http.Client))
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
			locale:      "pt-BR",
			email:       email,
			country:     "BR",
			setup: func(
				_ context.Context,
				t *testing.T,
				dataDir string,
				logger *slogWriter,
			) (kindling.Kindling, func(k kindling.Kindling, client *http.Client)) {
				k := kindling.NewKindling(
					"radiance",
					kindling.WithPanicListener(reporting.PanicListener),
					kindling.WithLogWriter(logger),
					kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
				)

				return k, nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			kindlingLogger := &slogWriter{Logger: slog.Default()}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			k, after := tc.setup(ctx, t, dataDir, kindlingLogger)

			httpClientWithTimeout := k.NewHTTPClient()
			httpClientWithTimeout.Timeout = common.DefaultHTTPTimeout

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
				const size = 10 * 1000000
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
		})
	}
}
