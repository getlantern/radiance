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
	restartFn       func() error // optional hook invoked inside RestartService
	mu              sync.Mutex
}

func (s *stubPlatform) RestartService() error {
	s.mu.Lock()
	s.restartCalled = true
	fn := s.restartFn
	errRet := s.restartErr
	s.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return errRet
}

func (s *stubPlatform) PostServiceClose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postCloseCalled = true
}

func TestNewVPNClient(t *testing.T) {
	t.Run("nil logger defaults to slog.Default", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), nil, nil)
		require.NotNil(t, c)
		assert.Equal(t, slog.Default(), c.logger)
		assert.Equal(t, Disconnected, c.Status())
	})

	t.Run("custom logger is retained", func(t *testing.T) {
		logger := rlog.NoOpLogger()
		c := NewVPNClient(t.TempDir(), logger, nil)
		require.NotNil(t, c)
		assert.Equal(t, logger, c.logger)
	})
}

func TestStatus(t *testing.T) {
	t.Run("disconnected when no tunnel", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
		assert.Equal(t, Disconnected, c.Status())
		assert.False(t, c.isOpen())
	})

	t.Run("concurrent reads", func(t *testing.T) {
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
	})
}

func TestConnect(t *testing.T) {
	t.Run("already connected", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
		c.status.Store(Connected)
		c.tunnel = &tunnel{}

		err := c.Connect(BoxOptions{})
		assert.ErrorIs(t, err, ErrTunnelAlreadyConnected)
	})

	t.Run("transient state refused", func(t *testing.T) {
		for _, status := range []VPNStatus{Restarting, Connecting, Disconnecting} {
			t.Run(string(status), func(t *testing.T) {
				c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
				c.status.Store(status)
				c.tunnel = &tunnel{}

				err := c.Connect(BoxOptions{})
				require.Error(t, err)
				assert.Contains(t, err.Error(), string(status))
			})
		}
	})

	t.Run("cleans up stale tunnel", func(t *testing.T) {
		for _, status := range []VPNStatus{Disconnected, ErrorStatus} {
			t.Run(string(status), func(t *testing.T) {
				c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
				c.status.Store(status)
				c.tunnel = &tunnel{}

				// Connect fails because BoxOptions has no outbounds, but the stale
				// tunnel should be cleared first so the error comes from buildOptions.
				err := c.Connect(BoxOptions{BasePath: t.TempDir()})
				require.Error(t, err)
				assert.Contains(t, err.Error(), "no outbounds")
			})
		}
	})
}

func TestDisconnect_NoTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	assert.NoError(t, c.Disconnect())
}

func TestRestart(t *testing.T) {
	t.Run("no tunnel", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
		err := c.Restart(BoxOptions{})
		assert.ErrorIs(t, err, ErrTunnelNotConnected)
	})

	t.Run("tunnel not connected", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
		c.status.Store(Disconnected)
		c.tunnel = &tunnel{}

		err := c.Restart(BoxOptions{})
		assert.ErrorIs(t, err, ErrTunnelNotConnected)
	})

	t.Run("platform interface success", func(t *testing.T) {
		// While RestartService is in flight, VPNClient.Status() must report
		// Restarting — bridging the window where the old tunnel is torn down and
		// the new one has not yet reached Connected. Once RestartService returns
		// successfully, status reflects the new tunnel's Connected state — which a
		// real platform drives by calling VPNClient.Disconnect + Connect
		// internally. The stub simulates that via a direct setStatus(Connected).
		entered := make(chan struct{})
		release := make(chan struct{})
		p := &stubPlatform{}
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), p)
		c.status.Store(Connected)
		c.tunnel = &tunnel{}

		p.restartFn = func() error {
			close(entered)
			<-release
			c.setStatus(Connected, nil)
			return nil
		}

		done := make(chan error, 1)
		go func() { done <- c.Restart(BoxOptions{}) }()

		<-entered
		assert.Equal(t, Restarting, c.Status(), "status should report Restarting while RestartService runs")
		close(release)

		require.NoError(t, <-done)

		p.mu.Lock()
		assert.True(t, p.restartCalled)
		p.mu.Unlock()
		assert.Equal(t, Connected, c.Status(), "status should reflect the new tunnel after restart completes")
	})

	t.Run("platform interface error", func(t *testing.T) {
		restartErr := errors.New("restart failed")
		p := &stubPlatform{restartErr: restartErr}
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), p)
		c.status.Store(Connected)
		c.tunnel = &tunnel{}

		err := c.Restart(BoxOptions{})
		require.Error(t, err)
		assert.ErrorIs(t, err, restartErr)
		assert.Equal(t, ErrorStatus, c.Status())
	})
}

func TestSelectServer(t *testing.T) {
	t.Run("no tunnel", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
		err := c.SelectServer("some-tag")
		assert.ErrorIs(t, err, ErrTunnelNotConnected)
	})

	t.Run("tunnel disconnected", func(t *testing.T) {
		c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
		c.status.Store(Disconnected)
		c.tunnel = &tunnel{}

		err := c.SelectServer("some-tag")
		assert.ErrorIs(t, err, ErrTunnelNotConnected)
	})
}

func TestNoTunnelOperations(t *testing.T) {
	ops := map[string]func(*VPNClient) error{
		"UpdateOutbounds": func(c *VPNClient) error { return c.UpdateOutbounds(servers.ServerList{}) },
		"AddOutbounds":    func(c *VPNClient) error { return c.AddOutbounds(servers.ServerList{}) },
		"RemoveOutbounds": func(c *VPNClient) error { return c.RemoveOutbounds([]string{"tag1"}) },
		"Connections": func(c *VPNClient) error {
			_, err := c.Connections()
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
			assert.ErrorIs(t, op(c), ErrTunnelNotConnected)
		})
	}
}

func TestCurrentAutoSelectedServer_NotOpen(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	selected, err := c.CurrentAutoSelectedServer()
	assert.NoError(t, err)
	assert.Empty(t, selected)
}

func TestRunOfflineURLTests_AlreadyConnected(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	c.status.Store(Connected)
	c.tunnel = &tunnel{}

	_, err := c.RunOfflineURLTests("", nil, nil)
	assert.ErrorIs(t, err, ErrTunnelAlreadyConnected)
}
