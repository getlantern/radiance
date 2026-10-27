package vpn

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

func TestConfigRefreshPreservesInstalledRouteTargets(t *testing.T) {
	for _, kind := range []string{"default", "logical", "final"} {
		t.Run(kind, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(target.Close)
			port := uint16(target.Listener.Addr().(*net.TCPAddr).Port)
			action := O.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: O.RouteActionOptions{Outbound: "routed"}}
			match := O.Rule{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{Port: []uint16{port}},
				RuleAction:     action,
			}}
			route := &O.RouteOptions{Final: "working"}
			switch kind {
			case "default":
				route.Rules = []O.Rule{match}
			case "logical":
				match.DefaultOptions.RuleAction = O.RuleAction{}
				route.Rules = []O.Rule{{Type: C.RuleTypeLogical, LogicalOptions: O.LogicalRule{
					RawLogicalRule: O.RawLogicalRule{Mode: C.LogicalTypeAnd, Rules: []O.Rule{match}},
					RuleAction:     action,
				}}}
			case "final":
				route.Final = "routed"
			}
			route.Rules = append(route.Rules, O.Rule{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{ClashMode: AutoSelectTag, Port: []uint16{1}},
				RuleAction:     O.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: O.RouteActionOptions{Outbound: "working"}},
			}})
			listenPort := freePort(t)
			probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(probe.Close)
			listen := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
			options := O.Options{
				Log: &O.LogOptions{Disabled: true},
				Inbounds: []O.Inbound{{Tag: "mixed", Type: C.TypeMixed, Options: &O.HTTPMixedInboundOptions{
					ListenOptions: O.ListenOptions{Listen: &listen, ListenPort: listenPort},
				}}},
				Outbounds: []O.Outbound{
					{Tag: "helper", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
					{Tag: "routed", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"helper"}}},
					{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
					{Tag: ManualSelectTag, Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"routed", "working"}}},
					{Tag: AutoSelectTag, Type: lbC.TypeMutableAutoSelect, Options: &lbO.MutableAutoSelectOutboundOptions{Outbounds: []string{"routed", "working"}, URL: probe.URL}},
				},
				Route: route,
				Experimental: &O.ExperimentalOptions{
					ClashAPI:  &O.ClashAPIOptions{DefaultMode: AutoSelectTag},
					CacheFile: &O.CacheFileOptions{Enabled: true, Path: filepath.Join(t.TempDir(), "cache.db")},
				},
			}
			tun := &tunnel{dataPath: t.TempDir(), initialNonSelectable: []string{"helper"}}
			t.Cleanup(func() { assert.NoError(t, tun.close()) })
			require.NoError(t, tun.init(t.Context(), options, nil))
			require.NoError(t, tun.boxInstance.Start())
			tun.optsMap = makeOutboundOptsMap(tun.ctx, options)
			tun.nonSelectableTags = makeNonSelectableTagSet(tun.initialNonSelectable, options)
			tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
			tun.clashServer = service.FromContext[adapter.ClashServer](tun.ctx).(*clashServer)
			manager, err := newMutableGroupManager(tun.ctx, tun.logFactory.NewLogger("groupsManager"), tun.clashServer.connTracker)
			require.NoError(t, err)
			tun.mutGrpMgr = manager
			t.Cleanup(manager.Close)
			transport := &http.Transport{
				Proxy:             http.ProxyURL(&url.URL{Scheme: "socks5", Host: fmt.Sprintf("127.0.0.1:%d", listenPort)}),
				DisableKeepAlives: true,
			}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			request := func() {
				response, err := client.Get(target.URL)
				require.NoError(t, err)
				response.Body.Close()
				assert.Equal(t, http.StatusNoContent, response.StatusCode)
			}
			working := servers.ServerList{Servers: []*servers.Server{{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
				Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
			}}}}
			request()
			require.NoError(t, tun.updateConfigOutbounds(working, servers.ServerList{}))
			for _, tag := range []string{ManualSelectTag, AutoSelectTag} {
				group, found := tun.mutGrpMgr.OutboundGroup(tag)
				require.True(t, found)
				assert.Equal(t, []string{"working"}, group.All())
			}
			for _, tag := range []string{"routed", "helper"} {
				_, found := tun.outboundMgr.Outbound(tag)
				require.True(t, found, tag)
			}
			clientState := &VPNClient{tunnel: tun}
			infrastructure, err := clientState.NonSelectableServers()
			require.NoError(t, err)
			assert.NotContains(t, infrastructure.Tags(), "routed")
			assert.Contains(t, infrastructure.Tags(), "helper")
			request()
			original, _ := tun.outboundMgr.Outbound("routed")
			working.Servers = append(working.Servers, &servers.Server{Tag: "routed", Type: lbC.TypeMutableSelector, Options: O.Outbound{
				Tag: "routed", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"working"}},
			}})
			require.NoError(t, tun.updateConfigOutbounds(working, servers.ServerList{}))
			replacement, found := tun.outboundMgr.Outbound("routed")
			require.True(t, found)
			assert.NotSame(t, original, replacement)
			_, found = tun.outboundMgr.Outbound("helper")
			assert.False(t, found)
			request()
		})
	}
}
