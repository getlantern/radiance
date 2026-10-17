package unbounded

import (
	"context"
	"encoding/json"
	"sync"
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

func newBytesManager(now *time.Time) (*unboundedManager, *slotTable) {
	slots := &slotTable{}
	return &unboundedManager{running: true, slots: slots, now: func() time.Time { return *now }}, slots
}

func peerBytesByIP(s Snapshot) map[string]PeerBytes {
	out := make(map[string]PeerBytes)
	for _, p := range s.PeerBytes {
		out[p.IP] = p
	}
	return out
}

func TestSnapshotAttributesBytesToPeerIPs(t *testing.T) {
	now := time.Unix(1000, 0)
	m, slots := newBytesManager(&now)
	ctx := context.Background()
	down, up := chunkCounter(ctx, slots, true), chunkCounter(ctx, slots, false)

	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	m.recordConnection(ctx, 1, 1, "192.0.2.2")
	m.recordConnection(ctx, 1, 2, "192.0.2.1")
	down(100, 0)
	down(50, 2)
	up(7, 0)
	down(30, 1)
	up(3, 1)
	// No known IP for slot 9, so these count only toward the totals.
	down(1000, 9)
	up(1, 9)
	down(-5, 0)

	s := m.snapshot()
	require.Equal(t, uint64(1180), s.BytesDown)
	require.Equal(t, uint64(11), s.BytesUp)
	require.Equal(t, []PeerBytes{
		{IP: "192.0.2.1", Down: 150, Up: 7},
		{IP: "192.0.2.2", Down: 30, Up: 3},
	}, s.PeerBytes)

	down(10, 1)
	s = m.snapshot()
	require.Equal(t, uint64(1190), s.BytesDown)
	require.Equal(t, uint64(40), peerBytesByIP(s)["192.0.2.2"].Down)
}

func TestSnapshotSlotReuseByDifferentIP(t *testing.T) {
	now := time.Unix(1000, 0)
	m, slots := newBytesManager(&now)
	ctx := context.Background()
	down := chunkCounter(ctx, slots, true)

	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	down(100, 0)
	m.recordConnection(ctx, -1, 0, "192.0.2.1")
	// Chunks between close and the next accept are late batches from the closed peer.
	down(5, 0)
	m.recordConnection(ctx, 1, 0, "192.0.2.2")
	down(40, 0)
	// An accept without a preceding close still hands the slot over cleanly.
	m.recordConnection(ctx, 1, 0, "192.0.2.3")
	down(2, 0)

	s := m.snapshot()
	require.Equal(t, uint64(147), s.BytesDown)
	got := peerBytesByIP(s)
	require.Equal(t, uint64(105), got["192.0.2.1"].Down)
	require.Equal(t, uint64(40), got["192.0.2.2"].Down)
	require.Equal(t, uint64(2), got["192.0.2.3"].Down)
	require.Equal(t, 0, m.peerBytes["192.0.2.2"].active)
	require.Equal(t, 1, m.peerBytes["192.0.2.3"].active)
}

func TestSnapshotRetainsEndedPeersFor60s(t *testing.T) {
	now := time.Unix(1000, 0)
	m, slots := newBytesManager(&now)
	ctx := context.Background()
	down := chunkCounter(ctx, slots, true)

	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	m.recordConnection(ctx, 1, 1, "192.0.2.1")
	m.recordConnection(ctx, 1, 2, "192.0.2.2")
	down(10, 0)
	down(20, 2)
	m.recordConnection(ctx, -1, 0, "")
	m.recordConnection(ctx, -1, 2, "")

	now = now.Add(2 * peerRetention)
	// 192.0.2.1 still holds slot 1, so it's kept regardless of age.
	require.Len(t, m.snapshot().PeerBytes, 1)
	m.recordConnection(ctx, -1, 1, "")

	now = now.Add(peerRetention)
	s := m.snapshot()
	require.Equal(t, []PeerBytes{{IP: "192.0.2.1", Down: 10}}, s.PeerBytes)
	now = now.Add(time.Second)
	s = m.snapshot()
	require.Empty(t, s.PeerBytes)
	require.Equal(t, uint64(30), s.BytesDown)

	// A returning IP starts a fresh entry.
	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	down(4, 0)
	require.Equal(t, []PeerBytes{{IP: "192.0.2.1", Down: 4}}, m.snapshot().PeerBytes)
}

func TestSnapshotStopKeepsFinalPeerBytes(t *testing.T) {
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		return &fakeWidget{}, nil
	})
	now := time.Unix(1000, 0)
	manager.now = func() time.Time { return now }
	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	require.NoError(t, Apply())
	require.Eventually(t, func() bool { return CurrentSnapshot().Running }, time.Second, time.Millisecond)

	manager.mu.Lock()
	slots := manager.slots
	manager.mu.Unlock()
	ctx, stopCtx := context.WithCancel(context.Background())
	manager.recordConnection(ctx, 1, 0, "192.0.2.1")
	down := chunkCounter(ctx, slots, true)
	down(64, 0)

	require.NoError(t, Stop(context.Background()))
	stopCtx()
	// Late chunks from the stopped run are dropped.
	down(1000, 0)
	slots.slot(0).down.Add(1000)
	s := CurrentSnapshot()
	require.False(t, s.Running)
	require.Empty(t, s.Peers)
	require.Equal(t, uint64(64), s.BytesDown)
	require.Equal(t, []PeerBytes{{IP: "192.0.2.1", Down: 64}}, s.PeerBytes)
	now = now.Add(peerRetention + time.Second)
	require.Empty(t, CurrentSnapshot().PeerBytes)
}

func TestSnapshotJSONShape(t *testing.T) {
	now := time.Unix(1000, 0)
	m, slots := newBytesManager(&now)
	ctx := context.Background()
	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	chunkCounter(ctx, slots, true)(12, 0)
	chunkCounter(ctx, slots, false)(3, 0)

	raw, err := json.Marshal(m.snapshot())
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.ElementsMatch(t, []string{"epoch", "arrivals", "enabled", "running", "peers", "bytesDown", "bytesUp", "peerBytes"}, keys(got))
	require.Equal(t, float64(12), got["bytesDown"])
	require.Equal(t, float64(3), got["bytesUp"])
	require.Equal(t, []any{map[string]any{"ip": "192.0.2.1", "down": float64(12), "up": float64(3)}}, got["peerBytes"])

	raw, err = json.Marshal((&unboundedManager{}).snapshot())
	require.NoError(t, err)
	require.Contains(t, string(raw), `"peerBytes":[]`)

	// Payloads from older backends without byte fields still decode.
	var old Snapshot
	require.NoError(t, json.Unmarshal([]byte(`{"epoch":"1","arrivals":2,"enabled":true,"running":true,"peers":[]}`), &old))
	require.Zero(t, old.BytesDown)
	require.Empty(t, old.PeerBytes)
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSnapshotConcurrentChunksAreCountedOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	m, slots := newBytesManager(&now)
	ctx := context.Background()
	down := chunkCounter(ctx, slots, true)
	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	m.recordConnection(ctx, 1, 1, "192.0.2.2")

	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				down(1, w)
			}
		}(w)
	}
	for i := 0; i < 50; i++ {
		m.snapshot()
	}
	wg.Wait()
	s := m.snapshot()
	require.Equal(t, uint64(2000), s.BytesDown)
	require.Equal(t, []PeerBytes{{IP: "192.0.2.1", Down: 1000}, {IP: "192.0.2.2", Down: 1000}}, s.PeerBytes)
}

func TestSnapshotCreditsLateUpstreamBatchToClosedPeer(t *testing.T) {
	now := time.Unix(1000, 0)
	m, slots := newBytesManager(&now)
	ctx := context.Background()
	up := chunkCounter(ctx, slots, false)

	m.recordConnection(ctx, 1, 0, "192.0.2.1")
	up(10, 0)
	m.snapshot()
	m.recordConnection(ctx, -1, 0, "")
	m.snapshot()
	// broflake flushes the closed peer's pending upstream batch after the close.
	up(7, 0)
	now = now.Add(time.Second)
	require.Equal(t, []PeerBytes{{IP: "192.0.2.1", Up: 17}}, m.snapshot().PeerBytes)

	m.recordConnection(ctx, 1, 0, "192.0.2.2")
	up(3, 0)
	s := m.snapshot()
	require.Equal(t, uint64(20), s.BytesUp)
	got := peerBytesByIP(s)
	require.Equal(t, uint64(17), got["192.0.2.1"].Up)
	require.Equal(t, uint64(3), got["192.0.2.2"].Up)
}

func TestSlotTableCloseWaitsForInFlightAdds(t *testing.T) {
	slots := &slotTable{}
	slots.mu.RLock()
	closed := make(chan struct{})
	go func() {
		slots.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned while an add was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	slots.slot(0).down.Add(5)
	slots.mu.RUnlock()
	<-closed
	slots.add(0, 100, true)
	require.Equal(t, uint64(5), slots.slot(0).down.Load())
}
