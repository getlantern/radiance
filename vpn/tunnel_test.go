package vpn

import (
	"context"
	"testing"

	lsync "github.com/getlantern/common/sync"
	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/adapter/groups"
	sbA "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

func TestTunnelStatus(t *testing.T) {
	tun := &tunnel{}
	tun.status.Store(Disconnected)
	assert.Equal(t, Disconnected, tun.Status())

	tun.setStatus(Connecting, nil)
	assert.Equal(t, Connecting, tun.Status())

	tun.setStatus(Connected, nil)
	assert.Equal(t, Connected, tun.Status())
}

func TestTunnelSetStatus_WithError(t *testing.T) {
	tun := &tunnel{}
	tun.status.Store(Disconnected)

	testErr := assert.AnError
	tun.setStatus(ErrorStatus, testErr)
	assert.Equal(t, ErrorStatus, tun.Status())
}

func TestTunnelClose_NoResources(t *testing.T) {
	tun := &tunnel{}
	tun.status.Store(Connected)
	err := tun.close()
	assert.NoError(t, err)
	assert.Equal(t, Disconnected, tun.Status())
	assert.Nil(t, tun.closers)
	assert.Nil(t, tun.lbService)
}

func TestTunnelClose_PreservesRestartingStatus(t *testing.T) {
	tun := &tunnel{}
	tun.status.Store(Restarting)
	err := tun.close()
	assert.NoError(t, err)
	assert.Equal(t, Restarting, tun.Status(), "close should not override Restarting status")
}

func TestTunnelClose_WithCancel(t *testing.T) {
	tun := &tunnel{}
	tun.status.Store(Connected)
	ctx, cancel := context.WithCancel(context.Background())
	tun.cancel = cancel

	err := tun.close()
	assert.NoError(t, err)
	assert.Error(t, ctx.Err(), "context should be cancelled after close")
}

type errCloser struct{ err error }

func (c errCloser) Close() error { return c.err }

func TestTunnelClose_CloserErrors(t *testing.T) {
	tun := &tunnel{}
	tun.status.Store(Connected)
	tun.closers = append(tun.closers, errCloser{err: assert.AnError})

	err := tun.close()
	assert.ErrorIs(t, err, assert.AnError)
}

func TestSelectMode_NotConnected(t *testing.T) {
	tun := &tunnel{}
	tun.status.Store(Disconnected)
	err := tun.selectMode(AutoSelectTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tunnel not running")
}

func TestRemoveDuplicates(t *testing.T) {
	ctx := box.BaseContext()

	out1 := O.Outbound{Type: "http", Tag: "http-1", Options: &O.HTTPOutboundOptions{}}
	out2 := O.Outbound{Type: "http", Tag: "http-2", Options: &O.HTTPOutboundOptions{}}
	ep1 := O.Endpoint{Type: "wireguard", Tag: "wg-1", Options: &O.WireGuardEndpointOptions{}}

	// Build a current map with out1 and ep1.
	var curr lsync.TypedMap[string, []byte]
	b1, _ := json.MarshalContext(ctx, out1)
	curr.Store(out1.Tag, b1)
	bEp1, _ := json.MarshalContext(ctx, ep1)
	curr.Store(ep1.Tag, bEp1)

	list := servers.ServerList{
		Servers: []*servers.Server{
			{Tag: out1.Tag, Type: out1.Type, Options: out1},
			{Tag: out2.Tag, Type: out2.Type, Options: out2},
			{Tag: ep1.Tag, Type: ep1.Type, Options: ep1},
		},
	}

	result := removeDuplicates(ctx, &curr, list)

	// out1 and ep1 are duplicates, only out2 should remain.
	assert.Len(t, result.Servers, 1)
	assert.Equal(t, "http-2", result.Servers[0].Tag)
}

func TestRemoveDuplicates_AllNew(t *testing.T) {
	ctx := box.BaseContext()
	var curr lsync.TypedMap[string, []byte]

	out1 := O.Outbound{Type: "http", Tag: "http-1", Options: &O.HTTPOutboundOptions{}}
	out2 := O.Outbound{Type: "socks", Tag: "socks-1", Options: &O.SOCKSOutboundOptions{}}

	list := servers.ServerList{
		Servers: []*servers.Server{
			{Tag: out1.Tag, Type: out1.Type, Options: out1},
			{Tag: out2.Tag, Type: out2.Type, Options: out2},
		},
	}

	result := removeDuplicates(ctx, &curr, list)
	assert.Len(t, result.Servers, 2)
}

func TestRemoveDuplicates_Empty(t *testing.T) {
	ctx := box.BaseContext()
	var curr lsync.TypedMap[string, []byte]

	result := removeDuplicates(ctx, &curr, servers.ServerList{})
	assert.Empty(t, result.Servers)
}

func TestContextDone(t *testing.T) {
	ctx := context.Background()
	assert.False(t, contextDone(ctx))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, contextDone(ctx))
}

type fakeConnMgr struct{}

func (fakeConnMgr) Connections() []trafficontrol.TrackerMetadata { return nil }

// TestTunnelClose_ClosesMutableGroupManager verifies that when a tunnel closes, the
// MutableGroupManager registered via t.closers is closed too. If the tunnel forgets
// to register this closer, the mgm's removalQueue goroutine outlives the tunnel and
// fires Remove on the already-closed sing-box OutboundManager, which panics inside
// sing-box-minimal. Regression: Freshdesk #173359, #173158.
func TestTunnelClose_ClosesMutableGroupManager(t *testing.T) {
	// Minimal MutableGroupManager — nil outbound/endpoint managers are safe here because
	// RemoveFromGroup's closed-check runs before touching them, and we never enqueue
	// anything so the removalQueue goroutine is never started.
	mgm := groups.NewMutableGroupManager(
		logger.NOP(),
		sbA.OutboundManager(nil),
		sbA.EndpointManager(nil),
		fakeConnMgr{},
		nil,
	)

	tun := &tunnel{}
	tun.status.Store(Connected)
	tun.mutGrpMgr = mgm
	tun.closers = append(tun.closers, closerFunc(func() error { mgm.Close(); return nil }))

	require.NoError(t, tun.close())

	err := mgm.RemoveFromGroup("some-group", "some-tag")
	assert.ErrorIs(t, err, groups.ErrIsClosed,
		"MutableGroupManager must be closed when tunnel closes; otherwise its removalQueue outlives the tunnel and panics against the stale outbound manager")
}
