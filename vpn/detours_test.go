package vpn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rlog "github.com/getlantern/radiance/log"
)

func TestTunnelDropsUnavailableDetours(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	for _, source := range []string{"fresh", "cached"} {
		t.Run(source, func(t *testing.T) {
			input := BoxOptions{BasePath: t.TempDir(), Options: O.Options{Outbounds: detourOutbounds()}}
			ctx := box.Context(t.Context())
			original, err := json.MarshalContext(ctx, input)
			require.NoError(t, err)
			if source == "cached" {
				input, err = json.UnmarshalExtendedContext[BoxOptions](ctx, original)
				require.NoError(t, err)
			}
			options, err := buildOptions(input)
			require.NoError(t, err)
			for _, out := range input.Options.Outbounds {
				if out.Tag == "working" {
					assert.NotNil(t, findOutbound(options.Outbounds, out.Tag))
				} else {
					assert.Nil(t, findOutbound(options.Outbounds, out.Tag), out.Tag)
				}
			}
			after, err := json.MarshalContext(ctx, input)
			require.NoError(t, err)
			assert.JSONEq(t, string(original), string(after))
			findOutbound(options.Outbounds, AutoSelectTag).Options.(*lbO.MutableAutoSelectOutboundOptions).URL = probe.URL
			tun := &tunnel{dataPath: input.BasePath}
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

func TestOfflineURLTestsDropUnavailableDetours(t *testing.T) {
	var hits atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	ctx, cancel := context.WithTimeout(box.Context(t.Context()), 5*time.Second)
	defer cancel()
	outbounds := detourOutbounds()
	original, err := json.MarshalContext(ctx, outbounds)
	require.NoError(t, err)
	client := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	results, err := client.RunOfflineURLTests(ctx, "", outbounds, map[string]string{"working": probe.URL})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results, "working")
	assert.Positive(t, hits.Load())
	after, err := json.MarshalContext(ctx, outbounds)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(after))
}

func TestNormalizeOutboundsReconcilesEndpointDetours(t *testing.T) {
	input := O.Options{
		Outbounds: append(detourOutbounds(),
			O.Outbound{Tag: "via-endpoint", Type: C.TypeHTTP, Options: &O.HTTPOutboundOptions{
				DialerOptions: O.DialerOptions{Detour: "chained-endpoint"},
			}},
			O.Outbound{Tag: "valid-detour", Type: C.TypeHTTP, Options: O.HTTPOutboundOptions{
				DialerOptions: O.DialerOptions{Detour: "working-endpoint"},
			}},
		),
		Endpoints: []O.Endpoint{
			{Tag: "chained-endpoint", Type: C.TypeWireGuard, Options: &O.WireGuardEndpointOptions{
				DialerOptions: O.DialerOptions{Detour: "endpoint"},
			}},
			{Tag: "endpoint", Type: C.TypeWireGuard, Options: O.WireGuardEndpointOptions{
				DialerOptions: O.DialerOptions{Detour: "via-unbounded"},
			}},
			{Tag: "working-endpoint", Type: C.TypeWireGuard, Options: &O.WireGuardEndpointOptions{
				DialerOptions: O.DialerOptions{Detour: "working"},
			}},
		},
	}
	ctx := box.Context(t.Context())
	original, err := json.MarshalContext(ctx, input)
	require.NoError(t, err)
	options, _ := normalizeOutbounds(input, nil)
	require.Len(t, options.Outbounds, 2)
	assert.NotNil(t, findOutbound(options.Outbounds, "working"))
	valid := findOutbound(options.Outbounds, "valid-detour")
	require.NotNil(t, valid)
	assert.Equal(t, input.Outbounds[len(input.Outbounds)-1], *valid)
	require.Len(t, options.Endpoints, 1)
	assert.Equal(t, input.Endpoints[2], options.Endpoints[0])
	after, err := json.MarshalContext(ctx, input)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(after))
}

func detourOutbounds() []O.Outbound {
	return []O.Outbound{
		{Tag: "chained", Type: C.TypeHTTP, Options: &O.HTTPOutboundOptions{
			DialerOptions: O.DialerOptions{Detour: "via-reflex"},
			ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
		}},
		{Tag: "via-reflex", Type: C.TypeHTTP, Options: &O.HTTPOutboundOptions{
			DialerOptions: O.DialerOptions{Detour: "reflex"},
			ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
		}},
		{Tag: "via-unbounded", Type: C.TypeHTTP, Options: O.HTTPOutboundOptions{
			DialerOptions: O.DialerOptions{Detour: "unbounded"},
			ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
		}},
		{Tag: "reflex", Type: lbC.TypeReflex, Options: &lbO.ReflexOutboundOptions{}},
		{Tag: "unbounded", Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{}},
		{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
	}
}
