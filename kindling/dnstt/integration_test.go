//go:build +integration

package dnstt

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getlantern/dnstt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEachDNSConfig load the dnstt.yml.gz config in memory
// create a DNS tunnel and try to fetch a request for each one
// of them
func TestEachDNSConfig(t *testing.T) {
	t.Parallel()
	configs, err := processYaml(embeddedConfig)
	require.NoError(t, err)
	ctx := context.Background()
	t.Logf("n configs to test: %d", len(configs))

	for _, cfg := range configs {
		var testname strings.Builder
		testname.WriteString("testing ")

		if cfg.DoHResolver != nil {
			testname.WriteString("DoHResolver ")
			testname.WriteString(*cfg.DoHResolver)
		}

		if cfg.DoTResolver != nil {
			testname.WriteString("DoTResolver ")
			testname.WriteString(*cfg.DoTResolver)
		}
		t.Run(testname.String(), func(t *testing.T) {
			opts := make([]dnstt.Option, 0)
			if cfg.Domain != "" {
				opts = append(opts, dnstt.WithTunnelDomain(cfg.Domain))
			}
			if cfg.PublicKey != "" {
				opts = append(opts, dnstt.WithPublicKey(cfg.PublicKey))
			}
			if cfg.DoHResolver != nil {
				t.Logf("testing %s", *cfg.DoHResolver)
				opts = append(opts, dnstt.WithDoH(*cfg.DoHResolver))
			}
			if cfg.DoTResolver != nil {
				t.Logf("testing %s", *cfg.DoTResolver)
				opts = append(opts, dnstt.WithDoT(*cfg.DoTResolver))
			}
			if cfg.UTLSDistribution != nil {
				opts = append(opts, dnstt.WithUTLSDistribution(*cfg.UTLSDistribution))
			}

			d, err := dnstt.NewDNSTT(opts...)
			require.NoError(t, err)
			rt, err := d.NewRoundTripper(ctx, "")
			require.NoError(t, err)

			cli := &http.Client{
				Transport: rt,
				Timeout:   1 * time.Minute,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.gstatic.com/generate_204", http.NoBody)
			require.NoError(t, err)
			resp, err := cli.Do(req)
			require.NoError(t, err)
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		})
	}
}
