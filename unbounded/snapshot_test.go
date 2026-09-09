package unbounded

import (
	"context"
	"testing"
	"time"

	"github.com/getlantern/broflake/clientcore"
	"github.com/getlantern/radiance/common/settings"
	"github.com/stretchr/testify/require"
)

func TestSnapshotRestoresPeersAndRejectsCancelledCallbacks(t *testing.T) {
	m := &unboundedManager{running: true}
	ctx, cancel := context.WithCancel(context.Background())
	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	m.recordConnection(ctx, 1, 1, "192.0.2.1")
	s := m.snapshot()
	require.Equal(t, uint64(1), s.Arrivals)
	require.Equal(t, []string{"192.0.2.1", "192.0.2.1"}, s.Peers)
	s.Peers[0] = "changed"
	require.Equal(t, "192.0.2.1", m.snapshot().Peers[0])
	m.recordConnection(ctx, -1, 0, "")
	require.Len(t, m.snapshot().Peers, 1)
	m.recordConnection(ctx, -1, 1, "")
	m.recordConnection(ctx, 1, 1, "192.0.2.1")
	require.Equal(t, uint64(2), m.snapshot().Arrivals)
	cancel()
	m.recordConnection(ctx, 1, 2, "192.0.2.2")
	require.Len(t, m.snapshot().Peers, 1)
	m.running = false
	require.Empty(t, m.snapshot().Peers)
}

func TestSnapshotReportsActualLifecycle(t *testing.T) {
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		return &fakeWidget{}, nil
	})
	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	require.NoError(t, Apply())
	require.True(t, CurrentSnapshot().Enabled)
	require.False(t, CurrentSnapshot().Running)
	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	require.NoError(t, Apply())
	require.Eventually(t, func() bool { return CurrentSnapshot().Running }, time.Second, time.Millisecond)
	require.NoError(t, Stop(context.Background()))
	require.True(t, CurrentSnapshot().Enabled)
	require.False(t, CurrentSnapshot().Running)
	require.Empty(t, CurrentSnapshot().Peers)
}
