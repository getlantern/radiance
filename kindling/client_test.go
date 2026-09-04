package kindling

import (
	"net/http"
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

type fakePausable struct{ paused, resumed int }

func (f *fakePausable) Pause()  { f.paused++ }
func (f *fakePausable) Resume() { f.resumed++ }

func TestClientPauseResumeDelegates(t *testing.T) {
	p := &fakePausable{}
	c := &Client{pausers: []pausable{p}}

	c.Pause()
	c.Pause()
	c.Resume()

	assert.Equal(t, 2, p.paused)
	assert.Equal(t, 1, p.resumed)
}

func TestClientPauseResumeNoPausers(t *testing.T) {
	c := &Client{}
	// Must not panic with no pausable transports (e.g. the staging, proxyless build).
	c.Pause()
	c.Resume()
}
