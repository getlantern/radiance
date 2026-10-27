package vpn

import (
	"context"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
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

func TestAllowedUnboundedRequiresTLSIdentity(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	t.Cleanup(server.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	for _, test := range []struct {
		name       string
		ca         string
		serverName string
		allowed    bool
	}{
		{name: "missing identity"},
		{name: "missing CA", serverName: "example.com"},
		{name: "missing server name", ca: ca},
		{name: "malformed CA", ca: "not a PEM certificate", serverName: "example.com"},
		{name: "configured identity", ca: ca, serverName: "example.com", allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := lbO.UnboundedOutboundOptions{
				EgressCA: test.ca, EgressServerName: test.serverName,
				InsecureDoNotVerifyClientCert: true, InsecureDoNotVerifyDiscoveryCert: true,
			}
			original := input
			for _, value := range []any{input, &input} {
				out := O.Outbound{Tag: "unbounded", Type: lbC.TypeUnbounded, Options: value}
				outbounds := []O.Outbound{out}
				list := servers.ServerList{Servers: []*servers.Server{{Tag: out.Tag, Type: out.Type, Options: out}}}
				for _, got := range [][]O.Outbound{allowedOutbounds(outbounds), allowedServers(list).Outbounds()} {
					if !test.allowed {
						assert.Empty(t, got)
						continue
					}
					require.Len(t, got, 1)
					var normalized lbO.UnboundedOutboundOptions
					switch opts := got[0].Options.(type) {
					case lbO.UnboundedOutboundOptions:
						normalized = opts
					case *lbO.UnboundedOutboundOptions:
						normalized = *opts
					default:
						t.Fatalf("unexpected options type %T", opts)
					}
					assert.False(t, normalized.InsecureDoNotVerifyClientCert)
					assert.False(t, normalized.InsecureDoNotVerifyDiscoveryCert)
					assert.Equal(t, input.EgressCA, normalized.EgressCA)
					assert.Equal(t, input.EgressServerName, normalized.EgressServerName)
				}
				assert.Equal(t, []O.Outbound{out}, outbounds)
				assert.Equal(t, []string{out.Tag}, list.Tags())
				assert.Equal(t, original, input)
			}
		})
	}
}

func TestAllowedReflexRequiresTLSIdentity(t *testing.T) {
	input := lbO.ReflexOutboundOptions{CertPEM: "client-certificate", KeyPEM: "private-key"}
	original := input
	for _, value := range []any{input, &input, (*lbO.ReflexOutboundOptions)(nil)} {
		out := O.Outbound{Tag: "reflex", Type: lbC.TypeReflex, Options: value}
		outbounds := []O.Outbound{out}
		list := servers.ServerList{Servers: []*servers.Server{{Tag: out.Tag, Type: out.Type, Options: out}}}
		assert.Empty(t, allowedOutbounds(outbounds))
		assert.Empty(t, allowedServers(list).Outbounds())
		assert.Equal(t, []O.Outbound{out}, outbounds)
		assert.Equal(t, []string{out.Tag}, list.Tags())
		assert.Equal(t, original, input)
	}
}

func TestBuildOptionsDropsUnauthenticatedProxies(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	for _, source := range []string{"fresh", "cached"} {
		t.Run(source, func(t *testing.T) {
			input := O.Options{Outbounds: []O.Outbound{
				{Tag: "unbounded", Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{
					InsecureDoNotVerifyClientCert: true, InsecureDoNotVerifyDiscoveryCert: true,
				}},
				{Tag: "malformed-ca", Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{
					EgressCA: "not a PEM certificate", EgressServerName: "example.com",
					InsecureDoNotVerifyClientCert: true, InsecureDoNotVerifyDiscoveryCert: true,
				}},
				{Tag: "reflex", Type: lbC.TypeReflex, Options: &lbO.ReflexOutboundOptions{
					CertPEM: "client-certificate", KeyPEM: "private-key",
				}},
				{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
			}}
			if source == "cached" {
				ctx := box.Context(t.Context())
				cached, err := json.MarshalContext(ctx, input)
				require.NoError(t, err)
				input, err = json.UnmarshalExtendedContext[O.Options](ctx, cached)
				require.NoError(t, err)
			}
			path := t.TempDir()
			options, err := buildOptions(BoxOptions{BasePath: path, Options: input})
			require.NoError(t, err)
			var groups int
			for _, out := range options.Outbounds {
				require.NotEqual(t, "unbounded", out.Tag)
				require.NotEqual(t, "malformed-ca", out.Tag)
				require.NotEqual(t, "reflex", out.Tag)
				switch opts := out.Options.(type) {
				case *lbO.MutableSelectorOutboundOptions:
					assert.Equal(t, []string{"working"}, opts.Outbounds)
					groups++
				case *lbO.MutableAutoSelectOutboundOptions:
					assert.Equal(t, []string{"working"}, opts.Outbounds)
					opts.URL = probe.URL
					groups++
				}
			}
			assert.Equal(t, 2, groups)
			require.Len(t, input.Outbounds, 4)
			for _, out := range input.Outbounds[:2] {
				original := out.Options.(*lbO.UnboundedOutboundOptions)
				assert.True(t, original.InsecureDoNotVerifyClientCert)
				assert.True(t, original.InsecureDoNotVerifyDiscoveryCert)
			}

			tun := &tunnel{dataPath: path}
			t.Cleanup(func() { assert.NoError(t, tun.close()) })
			require.NoError(t, tun.init(t.Context(), O.Options{
				Log:       &O.LogOptions{Disabled: true},
				Outbounds: options.Outbounds,
				Route: &O.RouteOptions{
					Rules: []O.Rule{selectModeRule(AutoSelectTag), selectModeRule(ManualSelectTag)},
					Final: "working",
				},
				Experimental: options.Experimental,
			}, nil))
			require.NoError(t, tun.boxInstance.Start())
		})
	}
}

func TestLiveOutboundsDropUnauthenticatedProxies(t *testing.T) {
	for _, operation := range []string{"add", "update", "non-selectable"} {
		t.Run(operation, func(t *testing.T) {
			tun := startPolicyTestTunnel(t)
			tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
			input := &lbO.UnboundedOutboundOptions{
				InsecureDoNotVerifyClientCert: true, InsecureDoNotVerifyDiscoveryCert: true,
			}
			list := servers.ServerList{Servers: []*servers.Server{
				{Tag: "unbounded", Type: lbC.TypeUnbounded, Options: O.Outbound{
					Tag: "unbounded", Type: lbC.TypeUnbounded, Options: input,
				}},
				{Tag: "reflex", Type: lbC.TypeReflex, Options: O.Outbound{
					Tag: "reflex", Type: lbC.TypeReflex, Options: &lbO.ReflexOutboundOptions{
						CertPEM: "client-certificate", KeyPEM: "private-key",
					},
				}},
				{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
					Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
				}},
			}}
			var err error
			switch operation {
			case "add":
				err = tun.addOutbounds(list)
			case "update":
				err = tun.updateOutbounds(list)
			case "non-selectable":
				err = tun.updateNonSelectableOutbounds(list)
			}
			require.NoError(t, err)
			_, found := tun.outboundMgr.Outbound("unbounded")
			assert.False(t, found)
			_, found = tun.outboundMgr.Outbound("reflex")
			assert.False(t, found)
			_, found = tun.outboundMgr.Outbound("working")
			assert.True(t, found)
			for _, tag := range []string{ManualSelectTag, AutoSelectTag} {
				group, found := tun.mutGrpMgr.OutboundGroup(tag)
				require.True(t, found)
				assert.NotContains(t, group.All(), "unbounded")
				assert.NotContains(t, group.All(), "reflex")
				if operation == "non-selectable" {
					assert.Equal(t, []string{"direct-out"}, group.All())
				} else {
					assert.Contains(t, group.All(), "working")
				}
			}
			assert.Equal(t, []string{"unbounded", "reflex", "working"}, list.Tags())
			assert.True(t, input.InsecureDoNotVerifyClientCert)
			assert.True(t, input.InsecureDoNotVerifyDiscoveryCert)
		})
	}
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
