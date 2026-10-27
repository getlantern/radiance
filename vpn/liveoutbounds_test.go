package vpn

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	O "github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

func TestLiveOutboundGroupsNormalizeBeforeCreationAndDeduplication(t *testing.T) {
	for _, operation := range []string{"add", "update", "non-selectable"} {
		t.Run(operation, func(t *testing.T) {
			tun := startPolicyTestTunnel(t)
			tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
			groupOptions := &O.SelectorOutboundOptions{
				Outbounds: []string{"denied", "direct-out", "working"},
				Default:   "denied",
			}
			group := O.Outbound{Tag: "group", Type: C.TypeSelector, Options: groupOptions}
			list := servers.ServerList{Servers: []*servers.Server{
				{Tag: group.Tag, Type: group.Type, Options: group, IsLantern: true},
				{Tag: "denied", Type: lbC.TypeUnbounded, Options: O.Outbound{
					Tag: "denied", Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{},
				}},
				{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
					Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
				}},
			}}
			apply := tun.addOutbounds
			members := []string{"direct-out", "working"}
			switch operation {
			case "update":
				apply = tun.updateOutbounds
				members = []string{"working"}
			case "non-selectable":
				apply = tun.updateNonSelectableOutbounds
			}

			require.NoError(t, apply(list))
			outbound, ok := tun.outboundMgr.Outbound(group.Tag)
			require.True(t, ok)
			assert.Equal(t, members, outbound.(adapter.OutboundGroup).All())
			_, ok = tun.outboundMgr.Outbound("denied")
			assert.False(t, ok)
			stored, ok := tun.optsMap.Load(group.Tag)
			require.True(t, ok)
			group.Options = &O.SelectorOutboundOptions{Outbounds: members}
			assert.Equal(t, marshalOptions(tun.ctx, group), stored)

			require.NoError(t, apply(list))
			reapplied, ok := tun.outboundMgr.Outbound(group.Tag)
			require.True(t, ok)
			assert.Same(t, outbound, reapplied)
			assert.Equal(t, []string{"denied", "direct-out", "working"}, groupOptions.Outbounds)
			assert.Equal(t, "denied", groupOptions.Default)
			assert.Equal(t, []string{"group", "denied", "working"}, list.Tags())
			assert.True(t, list.Servers[0].IsLantern)
		})
	}
}

func TestConfigRefreshChangesSelectableRole(t *testing.T) {
	tun := startPolicyTestTunnel(t)
	tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	working := &servers.Server{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
		Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
	}}
	changing := &servers.Server{Tag: "changing", Type: lbC.TypeMutableSelector, Options: O.Outbound{
		Tag: "changing", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"working"}},
	}}
	var original adapter.Outbound
	for _, helper := range []bool{false, true, false, true, false} {
		list := servers.ServerList{Servers: []*servers.Server{working}}
		var helpers servers.ServerList
		if helper {
			helpers.Servers = []*servers.Server{changing}
		} else {
			list.Servers = append(list.Servers, changing)
		}
		require.NoError(t, tun.updateConfigOutbounds(list, helpers))
		current, found := tun.outboundMgr.Outbound(changing.Tag)
		require.True(t, found)
		if original == nil {
			original = current
		}
		assert.Same(t, original, current)
		for _, tag := range []string{ManualSelectTag, AutoSelectTag} {
			group, found := tun.mutGrpMgr.OutboundGroup(tag)
			require.True(t, found)
			assert.Equal(t, !helper, slices.Contains(group.All(), changing.Tag))
		}
		_, tracked := tun.nonSelectableTags[changing.Tag]
		assert.Equal(t, helper, tracked)
		_, cached := tun.optsMap.Load(changing.Tag)
		assert.True(t, cached)
		assert.NotContains(t, tun.pendingRemovalTags, changing.Tag)
		conn, err := current.DialContext(t.Context(), "tcp", M.ParseSocksaddr(target.Listener.Addr().String()))
		require.NoError(t, err)
		conn.Close()
	}
}

func TestLiveRefreshReplacesChangedGroupsAndTheirParents(t *testing.T) {
	for _, mutable := range []bool{false, true} {
		t.Run(map[bool]string{false: "selector", true: "mutable-selector"}[mutable], func(t *testing.T) {
			tun := startPolicyTestTunnel(t)
			tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
			list := servers.ServerList{Servers: []*servers.Server{
				{Tag: "parent", Type: C.TypeSelector, Options: O.Outbound{Tag: "parent", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{Outbounds: []string{"child"}}}},
				{Tag: "child", Type: C.TypeSelector, Options: O.Outbound{Tag: "child", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{Outbounds: []string{"removed", "working"}, Default: "removed"}}},
				{Tag: "removed", Type: C.TypeHTTP, Options: O.Outbound{Tag: "removed", Type: C.TypeHTTP, Options: &O.HTTPOutboundOptions{ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 1}}}},
				{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}}},
			}}
			if mutable {
				for _, srv := range list.Servers[:2] {
					out := srv.Options.(O.Outbound)
					members := out.Options.(*O.SelectorOutboundOptions).Outbounds
					out.Type = lbC.TypeMutableSelector
					out.Options = &lbO.MutableSelectorOutboundOptions{Outbounds: members}
					srv.Type, srv.Options = out.Type, out
				}
			}
			require.NoError(t, tun.updateOutbounds(list))
			manual, ok := tun.mutGrpMgr.OutboundGroup(ManualSelectTag)
			require.True(t, ok)
			require.True(t, manual.(Selector).SelectOutbound("parent"))
			oldParent, _ := tun.outboundMgr.Outbound("parent")
			oldChild, _ := tun.outboundMgr.Outbound("child")

			list.Servers = append(list.Servers[:2:2], list.Servers[3])
			require.NoError(t, tun.updateOutbounds(list))
			parent, _ := tun.outboundMgr.Outbound("parent")
			child, _ := tun.outboundMgr.Outbound("child")
			assert.NotSame(t, oldParent, parent)
			assert.NotSame(t, oldChild, child)
			assert.Equal(t, []string{"working"}, child.(adapter.OutboundGroup).All())
			assert.Equal(t, "parent", manual.Now())
			for _, tag := range []string{ManualSelectTag, AutoSelectTag} {
				group, found := tun.mutGrpMgr.OutboundGroup(tag)
				require.True(t, found)
				assert.ElementsMatch(t, []string{"parent", "child", "working"}, group.All())
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			defer server.Close()
			transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return manual.DialContext(ctx, network, M.ParseSocksaddr(address))
			}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			resp, err := client.Get(server.URL)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)
			require.NoError(t, tun.updateOutbounds(list))
			reapplied, _ := tun.outboundMgr.Outbound("parent")
			assert.Same(t, parent, reapplied)
			require.NoError(t, tun.updateOutbounds(servers.ServerList{Servers: list.Servers[2:]}))
			assert.Equal(t, []string{"working"}, manual.All())
		})
	}
}

func TestNonSelectableRefreshDropsRemovedDependencies(t *testing.T) {
	tun := startPolicyTestTunnel(t)
	tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
	require.NoError(t, tun.updateNonSelectableOutbounds(servers.ServerList{Servers: []*servers.Server{
		{Tag: "old", Type: C.TypeDirect, Options: O.Outbound{
			Tag: "old", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
		}},
	}}))
	group := O.Outbound{Tag: "group", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
		Outbounds: []string{"old", "direct-out"}, Default: "old",
	}}
	require.NoError(t, tun.updateNonSelectableOutbounds(servers.ServerList{Servers: []*servers.Server{
		{Tag: group.Tag, Type: group.Type, Options: group},
	}}))
	outbound, ok := tun.outboundMgr.Outbound(group.Tag)
	require.True(t, ok)
	assert.Equal(t, []string{"direct-out"}, outbound.(adapter.OutboundGroup).All())
	_, ok = tun.outboundMgr.Outbound("old")
	assert.False(t, ok)
	_, ok = tun.optsMap.Load("old")
	assert.False(t, ok)
	selector, ok := tun.mutGrpMgr.OutboundGroup(ManualSelectTag)
	require.True(t, ok)
	assert.Equal(t, []string{"direct-out"}, selector.All())
}

func TestConfigRefreshRetainsHelpersUntilDependentsAreRemoved(t *testing.T) {
	tun := startPolicyTestTunnel(t)
	tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
	tun.mutGrpMgr.Close()
	manager, err := newMutableGroupManager(tun.ctx, tun.logFactory.NewLogger("groupsManager"), fixedConnections{{Outbound: "old"}})
	require.NoError(t, err)
	tun.mutGrpMgr = manager
	t.Cleanup(manager.Close)
	helper := servers.ServerList{Servers: []*servers.Server{{Tag: "helper", Type: C.TypeDirect, Options: O.Outbound{
		Tag: "helper", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
	}}}}
	old := servers.ServerList{Servers: []*servers.Server{{Tag: "old", Type: C.TypeHTTP, Options: O.Outbound{
		Tag: "old", Type: C.TypeHTTP, Options: &O.HTTPOutboundOptions{
			DialerOptions: O.DialerOptions{Detour: "helper"}, ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
		},
	}}}}
	require.NoError(t, tun.updateConfigOutbounds(old, helper))
	working := servers.ServerList{Servers: []*servers.Server{{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
		Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
	}}}}
	require.NoError(t, tun.updateConfigOutbounds(working, servers.ServerList{}))
	_, exists := tun.outboundMgr.Outbound("helper")
	require.True(t, exists)
	assert.Contains(t, tun.nonSelectableTags, "helper")
	require.NoError(t, tun.outboundMgr.Remove("old"))
	require.NoError(t, tun.updateConfigOutbounds(working, servers.ServerList{}))
	_, exists = tun.outboundMgr.Outbound("helper")
	assert.False(t, exists)
	_, exists = tun.optsMap.Load("helper")
	assert.False(t, exists)
	assert.NotContains(t, tun.nonSelectableTags, "helper")
}

func TestConfigRefreshRetiresGroupsAfterConnectionsDrain(t *testing.T) {
	for _, mutable := range []bool{false, true} {
		t.Run(map[bool]string{false: "selector", true: "mutable-urltest"}[mutable], func(t *testing.T) {
			tun := startPolicyTestTunnel(t)
			tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
			tun.clashServer = service.FromContext[adapter.ClashServer](tun.ctx).(*clashServer)
			probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(probe.Close)
			helper := servers.ServerList{Servers: []*servers.Server{{Tag: "helper", Type: C.TypeDirect, Options: O.Outbound{
				Tag: "helper", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
			}}}}
			child := O.Outbound{Tag: "child", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{Outbounds: []string{"helper"}}}
			if mutable {
				child.Type = lbC.TypeMutableURLTest
				child.Options = &lbO.MutableURLTestOutboundOptions{Outbounds: []string{"helper"}, URL: probe.URL}
			}
			old := servers.ServerList{Servers: []*servers.Server{
				{Tag: "parent", Type: C.TypeSelector, Options: O.Outbound{Tag: "parent", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{Outbounds: []string{child.Tag}}}},
				{Tag: child.Tag, Type: child.Type, Options: child},
			}}
			require.NoError(t, tun.updateConfigOutbounds(old, helper))
			parent, found := tun.outboundMgr.Outbound("parent")
			require.True(t, found)
			retiredChild, found := tun.outboundMgr.Outbound("child")
			require.True(t, found)
			require.NoError(t, tun.removeOutbounds([]string{"child"}))
			_, found = tun.outboundMgr.Outbound("child")
			require.True(t, found)

			tracker := tun.clashServer.connTracker
			conn := tun.clashServer.newRecord(adapter.InboundContext{}, nil, parent)
			tracker.join(conn)
			t.Cleanup(func() {
				if _, active := tracker.conns.Load(conn.id); active {
					tracker.leave(conn)
				}
			})
			working := servers.ServerList{Servers: []*servers.Server{{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
				Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
			}}}}
			require.NoError(t, tun.updateConfigOutbounds(working, servers.ServerList{}))
			for _, tag := range []string{"parent", "child", "helper"} {
				_, exists := tun.outboundMgr.Outbound(tag)
				require.True(t, exists, tag)
			}

			tracker.leave(conn)
			require.NoError(t, tun.updateConfigOutbounds(working, servers.ServerList{}))
			for _, tag := range []string{"parent", "child", "helper"} {
				_, exists := tun.outboundMgr.Outbound(tag)
				assert.False(t, exists, tag)
				_, exists = tun.optsMap.Load(tag)
				assert.False(t, exists, tag)
				assert.NotContains(t, tun.pendingRemovalTags, tag)
				assert.NotContains(t, tun.nonSelectableTags, tag)
			}
			if mutable {
				_, err := retiredChild.(lbA.MutableOutboundGroup).Add("working")
				assert.ErrorIs(t, err, lbA.ErrGroupClosed)
			}
		})
	}
}

type fixedConnections []trafficontrol.TrackerMetadata

func (c fixedConnections) Connections() []trafficontrol.TrackerMetadata { return c }

func TestLiveOutboundCreationOrdersEndpointAndDetourDependencies(t *testing.T) {
	list := servers.ServerList{Servers: []*servers.Server{
		{Tag: "group", Type: C.TypeSelector, Options: O.Outbound{
			Tag: "group", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{Outbounds: []string{"proxy"}},
		}},
		{Tag: "proxy", Type: C.TypeHTTP, Options: O.Outbound{
			Tag: "proxy", Type: C.TypeHTTP, Options: &O.HTTPOutboundOptions{
				DialerOptions: O.DialerOptions{Detour: "endpoint"},
				ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 8080},
			},
		}},
		{Tag: "endpoint", Type: C.TypeWireGuard, Options: O.Endpoint{
			Tag: "endpoint", Type: C.TypeWireGuard, Options: &O.WireGuardEndpointOptions{
				DialerOptions: O.DialerOptions{Detour: "direct-out"},
			},
		}},
	}}
	got := orderServersByDependencies(allowedServers(list, "direct-out"))
	assert.Equal(t, []string{"endpoint", "proxy", "group"}, got.Tags())
	assert.Equal(t, []string{"group", "proxy", "endpoint"}, list.Tags())
	assert.Equal(t, list.Servers[1].Options, got.Servers[1].Options)
	assert.Equal(t, list.Servers[2].Options, got.Servers[0].Options)
}
