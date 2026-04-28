package kindling

import (
	"net/http"
	"sync"
	"testing"

	"github.com/getlantern/kindling"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

			initOnce = sync.Once{}
			k = nil
			transport = nil
			closeTransports = nil

			newK, err := NewKindling(t.TempDir())
			require.NoError(t, err)
			require.NotNil(t, newK)
			SetKindling(newK)

			t.Cleanup(func() {
				Close()
				k = nil
				initOnce = sync.Once{}
			})

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
