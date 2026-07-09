package kindling

import (
	"net/http"
	"testing"

	"github.com/getlantern/kindling"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnableTransport(t *testing.T) {
	prev := EnabledTransports[kindling.TransportAMP]
	t.Cleanup(func() { EnabledTransports[kindling.TransportAMP] = prev })

	EnabledTransports[kindling.TransportAMP] = true
	assert.True(t, EnableTransport(kindling.TransportAMP, false), "flipping the value reports a change")
	assert.False(t, EnabledTransports[kindling.TransportAMP])
	assert.False(t, EnableTransport(kindling.TransportAMP, false), "setting the same value reports no change")
}

func TestNewClient(t *testing.T) {
	transports := []kindling.TransportName{
		kindling.TransportDomainfront,
		kindling.TransportSmart,
		kindling.TransportAMP,
	}

	for _, tr := range transports {
		t.Run(string(tr), func(t *testing.T) {
			for _, name := range transports {
				EnabledTransports[name] = false
			}
			EnabledTransports[kindling.TransportDNSTunnel] = false
			EnabledTransports[tr] = true

			Close()

			newK, err := NewKindling(t.TempDir())
			require.NoError(t, err)
			require.NotNil(t, newK)
			SetKindling(newK)

			t.Cleanup(func() { Close() })

			cli := HTTPClient()
			require.NotNil(t, cli)

			req, err := http.NewRequest(http.MethodPost, "https://df.iantem.io/api/v1/config-new", http.NoBody)
			require.NoError(t, err)

			resp, err := cli.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.NotNil(t, resp)
		})
	}
}
