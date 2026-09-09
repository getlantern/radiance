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
	// Must not panic with no pausable transports.
	c.Pause()
	c.Resume()
}

// restorePackageState isolates tests that touch the shared instance and its
// held pause state.
func restorePackageState(t *testing.T) {
	t.Helper()
	mu.Lock()
	pauseMu.Lock()
	prevK, prevPaused, prevPauseApplied, prevInitialized, prevTransport := k, paused, pauseApplied, initialized, transport
	k, paused, pauseApplied, initialized, transport = nil, false, false, false, nil
	pauseMu.Unlock()
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		pauseMu.Lock()
		k, pauseApplied, initialized, transport = prevK, prevPauseApplied, prevInitialized, prevTransport
		paused = prevPaused
		pauseMu.Unlock()
		mu.Unlock()
	})
}

func TestPauseHeldForClientInstalledLater(t *testing.T) {
	restorePackageState(t)
	p := &fakePausable{}

	// No shared instance yet, so this only records the state.
	Pause()
	mu.Lock()
	setClient(&Client{pausers: []pausable{p}})
	mu.Unlock()

	assert.Equal(t, 1, p.paused, "a client installed while paused must start paused")
}

func TestSetClientSkipsAlreadyAppliedHeldPause(t *testing.T) {
	restorePackageState(t)
	p := &fakePausable{paused: 1}
	Pause()

	mu.Lock()
	setClient(&Client{pausers: []pausable{p}, pauseApplied: true})
	mu.Unlock()
	Resume()

	assert.Equal(t, 1, p.paused, "a construction-applied pause must not be applied again")
	assert.Equal(t, 1, p.resumed)
}

func TestPauseResumeDelegateToLiveClient(t *testing.T) {
	restorePackageState(t)
	p := &fakePausable{}
	mu.Lock()
	setClient(&Client{pausers: []pausable{p}})
	mu.Unlock()

	Pause()
	Resume()

	assert.Equal(t, 1, p.paused)
	assert.Equal(t, 1, p.resumed)
}

func TestPauseResumeIgnoreRedundantCalls(t *testing.T) {
	restorePackageState(t)
	p := &fakePausable{}
	mu.Lock()
	setClient(&Client{pausers: []pausable{p}})
	mu.Unlock()

	Pause()
	Pause()
	Resume()
	Resume()

	assert.Equal(t, 1, p.paused, "a redundant Pause must not reach the transports")
	assert.Equal(t, 1, p.resumed, "a redundant Resume must not reach the transports")
}

func TestCloseClearsHeldPause(t *testing.T) {
	restorePackageState(t)
	p := &fakePausable{}

	Pause()
	require.NoError(t, Close())
	mu.Lock()
	setClient(&Client{pausers: []pausable{p}})
	mu.Unlock()

	assert.Equal(t, 0, p.paused, "Close must not leak the pause into the next instance")
}

func TestResumeClearsHeldPause(t *testing.T) {
	restorePackageState(t)
	p := &fakePausable{}

	Pause()
	Resume()
	mu.Lock()
	setClient(&Client{pausers: []pausable{p}})
	mu.Unlock()

	assert.Equal(t, 0, p.paused, "a client installed after resume must start running")
}
