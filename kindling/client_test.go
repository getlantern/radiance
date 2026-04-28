package kindling

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient(t *testing.T) {
	transports := []string{"fronted", "proxyless", "amp"}

	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			for _, name := range transports {
				enabledTransports[name] = false
			}
			enabledTransports["dnstt"] = false
			enabledTransports[tr] = true

			initOnce = sync.Once{}
			k = nil
			transport = nil
			closeTransports = nil

			newK, err := NewKindling(t.TempDir())
			require.NoError(t, err)
			require.NotNil(t, newK)
			setKindling(newK)

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
