package vpn

import (
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/conntrack"

	"github.com/getlantern/radiance/vpn/ipc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainConnectionsNoConnections(t *testing.T) {
	// With no active connections, drainConnections should return immediately.
	origTimeout := DrainTimeout
	DrainTimeout = 1 * time.Second
	defer func() { DrainTimeout = origTimeout }()

	start := time.Now()
	drainConnections()
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 100*time.Millisecond, "should return immediately with no connections")
}

func TestDrainConnectionsWaitsForConnections(t *testing.T) {
	if !conntrack.Enabled {
		t.Skip("conntrack not enabled (need build tag with_conntrack)")
	}

	origTimeout := DrainTimeout
	origPoll := DrainPollInterval
	DrainTimeout = 5 * time.Second
	DrainPollInterval = 10 * time.Millisecond
	defer func() {
		DrainTimeout = origTimeout
		DrainPollInterval = origPoll
	}()

	// Create a tracked connection using conntrack.
	server, client := net.Pipe()
	defer server.Close()

	tracked, err := conntrack.NewConn(client)
	require.NoError(t, err)
	require.Equal(t, 1, conntrack.Count(), "should have 1 tracked connection")

	// Close the connection after a short delay to simulate graceful drain.
	drainDelay := 200 * time.Millisecond
	go func() {
		time.Sleep(drainDelay)
		tracked.Close()
	}()

	start := time.Now()
	drainConnections()
	elapsed := time.Since(start)

	assert.Equal(t, 0, conntrack.Count(), "all connections should be drained")
	assert.GreaterOrEqual(t, elapsed, drainDelay, "should have waited for connection to close")
	assert.Less(t, elapsed, DrainTimeout, "should not have waited the full timeout")
}

func TestDrainConnectionsTimeout(t *testing.T) {
	if !conntrack.Enabled {
		t.Skip("conntrack not enabled (need build tag with_conntrack)")
	}

	origTimeout := DrainTimeout
	origPoll := DrainPollInterval
	DrainTimeout = 200 * time.Millisecond
	DrainPollInterval = 10 * time.Millisecond
	defer func() {
		DrainTimeout = origTimeout
		DrainPollInterval = origPoll
	}()

	// Create a tracked connection that never closes.
	server, client := net.Pipe()
	defer server.Close()

	tracked, err := conntrack.NewConn(client)
	require.NoError(t, err)
	defer tracked.Close()
	require.Equal(t, 1, conntrack.Count(), "should have 1 tracked connection")

	start := time.Now()
	drainConnections()
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, DrainTimeout, "should have waited the full timeout")
	assert.Less(t, elapsed, DrainTimeout+100*time.Millisecond, "should not overshoot timeout significantly")
	assert.Equal(t, 1, conntrack.Count(), "connection should still be open after timeout")
}

func TestTunnelCloseCallsDrain(t *testing.T) {
	// Verify that tunnel.close() invokes the drain phase by checking that closing a tunnel
	// with no closers and no connections completes quickly.
	tun := &tunnel{}
	tun.status.Store(ipc.Disconnected)

	start := time.Now()
	err := tun.close()
	elapsed := time.Since(start)

	assert.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond, "close with no connections should be fast")
}
