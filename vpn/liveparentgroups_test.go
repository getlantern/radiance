package vpn

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	lcommon "github.com/getlantern/common"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

func TestLiveRefreshUpdatesInstalledSmartRoutingParents(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "smart-routing", true: "nested-parent"}[nested], func(t *testing.T) {
			tun := startPolicyTestTunnel(t)
			tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(target.Close)
			list := servers.ServerList{Servers: []*servers.Server{
				{Tag: "child", Type: lbC.TypeMutableSelector, Options: O.Outbound{
					Tag: "child", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"removed", "working"}},
				}},
				{Tag: "removed", Type: C.TypeHTTP, Options: O.Outbound{
					Tag: "removed", Type: C.TypeHTTP, Options: &O.HTTPOutboundOptions{ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 1}},
				}},
				{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
					Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
				}},
			}}
			require.NoError(t, tun.updateOutbounds(list))
			rules := lcommon.SmartRoutingRules{{Category: "live", Outbounds: []string{"child"}}}
			parents, _, _ := rules.ToOptions(urlTestInterval, urlTestIdleTimeout)
			parents[0].Options.(*O.URLTestOutboundOptions).URL = target.URL
			if nested {
				parents = append(parents, O.Outbound{Tag: "outer", Type: C.TypeSelector,
					Options: &O.SelectorOutboundOptions{Outbounds: []string{"sr-live"}},
				})
			}
			for _, out := range parents {
				require.NoError(t, tun.outboundMgr.Create(tun.ctx, service.FromContext[adapter.Router](tun.ctx),
					tun.logFactory.NewLogger(out.Tag), out.Tag, out.Type, out.Options))
				tun.optsMap.Store(out.Tag, marshalOptions(tun.ctx, out))
			}
			parentTag := parents[len(parents)-1].Tag
			transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				out, found := tun.outboundMgr.Outbound(parentTag)
				require.True(t, found)
				return out.DialContext(ctx, network, M.ParseSocksaddr(address))
			}}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			list.Servers = append(list.Servers[:1:1], list.Servers[2])
			for range 2 {
				require.NoError(t, tun.updateOutbounds(list))
				parent, found := tun.outboundMgr.Outbound("sr-live")
				require.True(t, found)
				checker := parent.(interface {
					URLTest(context.Context) (map[string]uint16, error)
				})
				clash := service.FromContext[adapter.ClashServer](tun.ctx)
				require.Eventually(t, func() bool {
					clash.HistoryStorage().DeleteURLTestHistory("working")
					result, err := checker.URLTest(t.Context())
					return err == nil && len(result) > 0
				}, time.Second, time.Millisecond)
				resp, err := client.Get(target.URL)
				require.NoError(t, err)
				resp.Body.Close()
				assert.Equal(t, http.StatusNoContent, resp.StatusCode)
				manual, found := tun.mutGrpMgr.OutboundGroup(ManualSelectTag)
				require.True(t, found)
				assert.NotContains(t, manual.All(), parentTag)
			}
		})
	}
}
