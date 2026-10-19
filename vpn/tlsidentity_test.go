package vpn

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	O "github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

func insecureHTTPOutbound(tag, server string, port uint16) O.Outbound {
	return O.Outbound{Tag: tag, Type: "http", Options: &O.HTTPOutboundOptions{
		ServerOptions: O.ServerOptions{Server: server, ServerPort: port},
		OutboundTLSOptionsContainer: O.OutboundTLSOptionsContainer{TLS: &O.OutboundTLSOptions{
			Enabled: true, Insecure: true, ServerName: "example.com",
		}},
	}}
}

func TestBuildOptionsRequiresTLSIdentity(t *testing.T) {
	out := insecureHTTPOutbound("managed", "example.com", 443)
	options, err := buildOptions(BoxOptions{BasePath: t.TempDir(), Options: O.Options{Outbounds: []O.Outbound{out}}})
	require.NoError(t, err)
	var found bool
	for _, got := range options.Outbounds {
		if got.Tag == out.Tag {
			found = true
			assert.False(t, got.Options.(*O.HTTPOutboundOptions).TLS.Insecure)
		}
	}
	require.True(t, found)
	assert.True(t, out.Options.(*O.HTTPOutboundOptions).TLS.Insecure)
}

func TestLiveOutboundsRequireTLSIdentity(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	address := M.ParseSocksaddr(server.Listener.Addr().String())
	tun := startPolicyTestTunnel(t)
	tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)

	for i, add := range []func(servers.ServerList) error{tun.addOutbounds, tun.updateNonSelectableOutbounds} {
		out := insecureHTTPOutbound("tls-identity-"+strconv.Itoa(i), address.AddrString(), address.Port)
		list := servers.ServerList{Servers: []*servers.Server{{Tag: out.Tag, Type: out.Type, IsLantern: i == 1, Options: out}}}
		require.NoError(t, add(list))
		outbound, found := service.FromContext[adapter.OutboundManager](tun.ctx).Outbound(out.Tag)
		require.True(t, found)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		conn, err := outbound.DialContext(ctx, "tcp", M.ParseSocksaddr("target.invalid:443"))
		cancel()
		if conn != nil {
			conn.Close()
		}
		require.ErrorContains(t, err, "certificate")
		assert.True(t, out.Options.(*O.HTTPOutboundOptions).TLS.Insecure)
	}
}
