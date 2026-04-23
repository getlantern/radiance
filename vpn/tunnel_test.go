package vpn

import (
	"context"
	"testing"

	lsync "github.com/getlantern/common/sync"
	box "github.com/getlantern/lantern-box"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

type errCloser struct{ err error }

func (c errCloser) Close() error { return c.err }

func TestTunnelClose(t *testing.T) {
	t.Run("no resources", func(t *testing.T) {
		tun := &tunnel{}
		err := tun.close()
		assert.NoError(t, err)
		assert.Nil(t, tun.closers)
		assert.Nil(t, tun.lbService)
	})

	t.Run("cancels context", func(t *testing.T) {
		tun := &tunnel{}
		ctx, cancel := context.WithCancel(context.Background())
		tun.cancel = cancel

		err := tun.close()
		assert.NoError(t, err)
		assert.Error(t, ctx.Err(), "context should be cancelled after close")
	})

	t.Run("propagates closer errors", func(t *testing.T) {
		tun := &tunnel{}
		tun.closers = append(tun.closers, errCloser{err: assert.AnError})

		err := tun.close()
		assert.ErrorIs(t, err, assert.AnError)
	})
}

func TestSelectMode_NotConnected(t *testing.T) {
	// A tunnel without an active libbox service is not running.
	tun := &tunnel{}
	err := tun.selectMode(AutoSelectTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tunnel not running")
}

func TestRemoveDuplicates(t *testing.T) {
	ctx := box.BaseContext()
	out1 := O.Outbound{Type: "http", Tag: "http-1", Options: &O.HTTPOutboundOptions{}}
	out2 := O.Outbound{Type: "http", Tag: "http-2", Options: &O.HTTPOutboundOptions{}}
	socks := O.Outbound{Type: "socks", Tag: "socks-1", Options: &O.SOCKSOutboundOptions{}}
	ep1 := O.Endpoint{Type: "wireguard", Tag: "wg-1", Options: &O.WireGuardEndpointOptions{}}

	t.Run("drops duplicates against current map", func(t *testing.T) {
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
		assert.Len(t, result.Servers, 1)
		assert.Equal(t, "http-2", result.Servers[0].Tag)
	})

	t.Run("keeps all servers when none are duplicates", func(t *testing.T) {
		var curr lsync.TypedMap[string, []byte]
		list := servers.ServerList{
			Servers: []*servers.Server{
				{Tag: out1.Tag, Type: out1.Type, Options: out1},
				{Tag: socks.Tag, Type: socks.Type, Options: socks},
			},
		}

		result := removeDuplicates(ctx, &curr, list)
		assert.Len(t, result.Servers, 2)
	})

	t.Run("empty list yields empty result", func(t *testing.T) {
		var curr lsync.TypedMap[string, []byte]
		result := removeDuplicates(ctx, &curr, servers.ServerList{})
		assert.Empty(t, result.Servers)
	})
}

func TestContextDone(t *testing.T) {
	ctx := context.Background()
	assert.False(t, contextDone(ctx))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, contextDone(ctx))
}
