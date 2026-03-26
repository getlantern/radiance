package vpn

import (
	"context"
	"testing"

	lcommon "github.com/getlantern/common"
	lsync "github.com/getlantern/common/sync"
	box "github.com/getlantern/lantern-box"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
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

	newOpts := servers.Options{
		Outbounds: []O.Outbound{out1, out2},
		Endpoints: []O.Endpoint{ep1},
		Locations: map[string]lcommon.ServerLocation{
			out1.Tag: {},
			out2.Tag: {},
			ep1.Tag:  {},
		},
	}

	result := removeDuplicates(ctx, &curr, newOpts)

	// out1 and ep1 are duplicates, only out2 should remain.
	assert.Len(t, result.Outbounds, 1)
	assert.Equal(t, "http-2", result.Outbounds[0].Tag)
	assert.Empty(t, result.Endpoints)
}

func TestRemoveDuplicates_AllNew(t *testing.T) {
	ctx := box.BaseContext()
	var curr lsync.TypedMap[string, []byte]

	out1 := O.Outbound{Type: "http", Tag: "http-1", Options: &O.HTTPOutboundOptions{}}
	out2 := O.Outbound{Type: "socks", Tag: "socks-1", Options: &O.SOCKSOutboundOptions{}}

	newOpts := servers.Options{
		Outbounds: []O.Outbound{out1, out2},
		Locations: map[string]lcommon.ServerLocation{
			out1.Tag: {},
			out2.Tag: {},
		},
	}

	result := removeDuplicates(ctx, &curr, newOpts)
	assert.Len(t, result.Outbounds, 2)
}

func TestRemoveDuplicates_Empty(t *testing.T) {
	ctx := box.BaseContext()
	var curr lsync.TypedMap[string, []byte]

	result := removeDuplicates(ctx, &curr, servers.Options{})
	assert.Empty(t, result.Outbounds)
	assert.Empty(t, result.Endpoints)
}

func TestContextDone(t *testing.T) {
	ctx := context.Background()
	assert.False(t, contextDone(ctx))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, contextDone(ctx))
}
