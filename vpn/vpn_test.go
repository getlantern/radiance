package vpn

import (
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rlog "github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
)

// stubPlatform implements PlatformInterface for testing without real VPN operations.
type stubPlatform struct {
	libbox.PlatformInterface

	restartErr      error
	restartCalled   bool
	postCloseCalled bool
	mu              sync.Mutex
}

func (s *stubPlatform) RestartService() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restartCalled = true
	return s.restartErr
}

func (s *stubPlatform) PostServiceClose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postCloseCalled = true
}

func TestNewVPNClient(t *testing.T) {
	t.Run("with nil logger uses default", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), nil, nil)
		require.NotNil(t, c)
		assert.Equal(t, slog.Default(), c.logger)
		assert.Equal(t, Disconnected, c.Status())
	})

	t.Run("with custom logger", func(t *testing.T) {
		logger := rlog.NoOpLogger()
		c := NewVPNClient(t.TempDir(), logger, nil)
		require.NotNil(t, c)
		assert.Equal(t, logger, c.logger)
	})
}

func TestStatus_DisconnectedWhenNoTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	assert.Equal(t, Disconnected, c.Status())
	assert.False(t, c.isOpen())
}

func TestClose_NilTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	// Closing when no tunnel is open should succeed without error.
	assert.NoError(t, c.Close())
}

func TestClose_CallsPostServiceClose(t *testing.T) {
	p := &stubPlatform{}
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), p)

	// Set up a minimal tunnel that can be closed.
	tun := &tunnel{}
	tun.status.Store(Connected)
	c.tunnel = tun

	err := c.Close()
	assert.NoError(t, err)
	assert.Nil(t, c.tunnel)

	p.mu.Lock()
	assert.True(t, p.postCloseCalled, "PostServiceClose should be called after closing")
	p.mu.Unlock()
}

func TestDisconnect_NoTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	assert.NoError(t, c.Disconnect())
}

func TestConnect_AlreadyConnected(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	tun := &tunnel{}
	tun.status.Store(Connected)
	c.tunnel = tun

	err := c.Connect(BoxOptions{})
	assert.ErrorIs(t, err, ErrTunnelAlreadyConnected)
}

func TestConnect_TransientStates(t *testing.T) {
	for _, status := range []VPNStatus{Restarting, Connecting, Disconnecting} {
		t.Run(string(status), func(t *testing.T) {
			c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
			tun := &tunnel{}
			tun.status.Store(status)
			c.tunnel = tun

			err := c.Connect(BoxOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), string(status))
		})
	}
}

func TestConnect_CleansUpStaleTunnel(t *testing.T) {
	for _, status := range []VPNStatus{Disconnected, ErrorStatus} {
		t.Run(string(status), func(t *testing.T) {
			c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
			tun := &tunnel{}
			tun.status.Store(status)
			c.tunnel = tun

			// Connect will fail because BoxOptions has no outbounds, but the stale
			// tunnel should be cleared first (the error comes from buildOptions).
			err := c.Connect(BoxOptions{BasePath: t.TempDir()})
			require.Error(t, err)
			// The tunnel should have been nilled out before buildOptions was called
			assert.Contains(t, err.Error(), "no outbounds")
		})
	}
}

func TestRestart_NotConnected(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	err := c.Restart(BoxOptions{})
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestRestart_NotConnectedStatus(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	tun := &tunnel{}
	tun.status.Store(Disconnected)
	c.tunnel = tun

	err := c.Restart(BoxOptions{})
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestRestart_WithPlatformInterface(t *testing.T) {
	p := &stubPlatform{}
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), p)
	tun := &tunnel{}
	tun.status.Store(Connected)
	c.tunnel = tun

	err := c.Restart(BoxOptions{})
	assert.NoError(t, err)

	p.mu.Lock()
	assert.True(t, p.restartCalled)
	p.mu.Unlock()
	assert.Equal(t, Restarting, tun.Status())
}

func TestRestart_PlatformInterfaceError(t *testing.T) {
	restartErr := errors.New("restart failed")
	p := &stubPlatform{restartErr: restartErr}
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), p)
	tun := &tunnel{}
	tun.status.Store(Connected)
	c.tunnel = tun

	err := c.Restart(BoxOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, restartErr)
	assert.Equal(t, ErrorStatus, tun.Status())
}

func TestSelectServer_NotConnected(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	err := c.SelectServer("some-tag")
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestSelectServer_DisconnectedTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	tun := &tunnel{}
	tun.status.Store(Disconnected)
	c.tunnel = tun

	err := c.SelectServer("some-tag")
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestUpdateOutbounds_NilTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	err := c.UpdateOutbounds(servers.ServerList{}, true)
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestAddOutbounds_NilTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	err := c.AddOutbounds(servers.ServerList{}, true)
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestRemoveOutbounds_NilTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	err := c.RemoveOutbounds([]string{"tag1"}, true)
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestConnections_NilTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	conns, err := c.Connections()
	assert.Nil(t, conns)
	assert.ErrorIs(t, err, ErrTunnelNotConnected)
}

func TestCurrentAutoSelectedServer_NotOpen(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	selected, err := c.CurrentAutoSelectedServer()
	assert.NoError(t, err)
	assert.Empty(t, selected)
}

func TestRunOfflineURLTests_AlreadyConnected(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	tun := &tunnel{}
	tun.status.Store(Connected)
	c.tunnel = tun

	_, err := c.RunOfflineURLTests("", nil, nil)
	assert.ErrorIs(t, err, ErrTunnelAlreadyConnected)
}

func TestConcurrentStatusAccess(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Status()
		}()
	}
	wg.Wait()
}
