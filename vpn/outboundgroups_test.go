package vpn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	lcommon "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTunnelConfiguredGroupsDropUnverifiableMembers(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	for _, source := range []string{"fresh", "cached"} {
		t.Run(source, func(t *testing.T) {
			input := BoxOptions{
				BasePath: t.TempDir(),
				Options: O.Options{Outbounds: []O.Outbound{
					{Tag: "unbounded", Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{
						InsecureDoNotVerifyClientCert: true,
					}},
					{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
					{Tag: "selector", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
						Outbounds: []string{"unbounded", "working"}, Default: "unbounded",
					}},
					{Tag: "urltest", Type: C.TypeURLTest, Options: &O.URLTestOutboundOptions{
						Outbounds: []string{"unbounded", "working"}, URL: probe.URL,
					}},
					{Tag: "mutable-selector", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{
						Outbounds: []string{"unbounded", "working"},
					}},
					{Tag: "mutable-urltest", Type: lbC.TypeMutableURLTest, Options: &lbO.MutableURLTestOutboundOptions{
						Outbounds: []string{"unbounded", "working"}, URL: probe.URL,
					}},
					{Tag: "mutable-auto", Type: lbC.TypeMutableAutoSelect, Options: &lbO.MutableAutoSelectOutboundOptions{
						Outbounds: []string{"unbounded", "working"}, URL: probe.URL,
					}},
					{Tag: "valid-default", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
						Outbounds: []string{"direct", "unbounded", "working"}, Default: "working",
					}},
					{Tag: "parent", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
						Outbounds: []string{"empty-parent", "sr-empty", "fallback-primary", "fallback-secondary", "fallback-parent", "working"}, Default: "empty-parent",
					}},
					{Tag: "smart-selector", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
						Outbounds: []string{"sr-live"}, Default: "sr-live",
					}},
					{Tag: "fallback-primary", Type: lbC.TypeFallback, Options: lbO.FallbackOutboundOptions{
						Primary: "unbounded", Fallback: "working",
					}},
					{Tag: "fallback-secondary", Type: lbC.TypeFallback, Options: &lbO.FallbackOutboundOptions{
						Primary: "working", Fallback: "unbounded",
					}},
					{Tag: "fallback-parent", Type: lbC.TypeFallback, Options: &lbO.FallbackOutboundOptions{
						Primary: "fallback-empty", Fallback: "working",
					}},
					{Tag: "fallback-empty", Type: lbC.TypeFallback, Options: &lbO.FallbackOutboundOptions{
						Primary: "unbounded", Fallback: "missing",
					}},
					{Tag: "fallback-working", Type: lbC.TypeFallback, Options: &lbO.FallbackOutboundOptions{
						Primary: "working", Fallback: "direct",
					}},
					{Tag: "empty-parent", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
						Outbounds: []string{"empty-child"},
					}},
					{Tag: "empty-child", Type: C.TypeURLTest, Options: &O.URLTestOutboundOptions{
						Outbounds: []string{"unbounded"}, URL: probe.URL,
					}},
				}},
				SmartRouting: lcommon.SmartRoutingRules{
					{Category: "live", Outbounds: []string{"working"}},
					{Category: "empty", Outbounds: []string{"empty-parent"}, RuleSets: lcommon.RuleSets{{Tag: "empty", URL: probe.URL}}},
				},
			}
			ctx := box.Context(t.Context())
			original, err := json.MarshalContext(ctx, input)
			require.NoError(t, err)
			if source == "cached" {
				input, err = json.UnmarshalExtendedContext[BoxOptions](ctx, original)
				require.NoError(t, err)
			}
			options, err := buildOptions(input)
			require.NoError(t, err)
			for _, tag := range []string{"unbounded", "empty-parent", "empty-child", "sr-empty", "fallback-primary", "fallback-secondary", "fallback-parent", "fallback-empty"} {
				assert.Nil(t, findOutbound(options.Outbounds, tag), tag)
			}
			fallback := findOutbound(options.Outbounds, "fallback-working")
			require.NotNil(t, fallback)
			assert.Equal(t, &lbO.FallbackOutboundOptions{Primary: "working", Fallback: "direct"}, fallback.Options)
			for _, rule := range options.Route.Rules {
				assert.NotEqual(t, "sr-empty", rule.DefaultOptions.RouteOptions.Outbound)
			}
			for _, ruleSet := range options.Route.RuleSet {
				assert.NotEqual(t, "empty", ruleSet.Tag)
			}
			for _, tag := range []string{"selector", "urltest", "mutable-selector", "mutable-urltest", "mutable-auto", "parent", "sr-live"} {
				out := findOutbound(options.Outbounds, tag)
				require.NotNil(t, out, tag)
				assert.Equal(t, []string{"working"}, configuredGroupMembers(t, *out), tag)
			}
			for _, tag := range []string{"selector", "parent"} {
				assert.Empty(t, findOutbound(options.Outbounds, tag).Options.(*O.SelectorOutboundOptions).Default)
			}
			validDefault := findOutbound(options.Outbounds, "valid-default").Options.(*O.SelectorOutboundOptions)
			assert.Equal(t, []string{"direct", "working"}, validDefault.Outbounds)
			assert.Equal(t, "working", validDefault.Default)
			smartSelector := findOutbound(options.Outbounds, "smart-selector")
			require.NotNil(t, smartSelector)
			assert.Equal(t, []string{"sr-live"}, configuredGroupMembers(t, *smartSelector))
			assert.Equal(t, "sr-live", smartSelector.Options.(*O.SelectorOutboundOptions).Default)
			after, err := json.MarshalContext(ctx, input)
			require.NoError(t, err)
			assert.JSONEq(t, string(original), string(after))
			findOutbound(options.Outbounds, AutoSelectTag).Options.(*lbO.MutableAutoSelectOutboundOptions).URL = probe.URL
			findOutbound(options.Outbounds, "sr-live").Options.(*O.URLTestOutboundOptions).URL = probe.URL
			tun := &tunnel{dataPath: input.BasePath}
			t.Cleanup(func() { assert.NoError(t, tun.close()) })
			require.NoError(t, tun.init(t.Context(), O.Options{
				Log:       &O.LogOptions{Disabled: true},
				Outbounds: options.Outbounds,
				Route: &O.RouteOptions{
					Rules: []O.Rule{selectModeRule(AutoSelectTag), selectModeRule(ManualSelectTag)},
					Final: "parent",
				},
				Experimental: options.Experimental,
			}, nil))
			require.NoError(t, tun.boxInstance.Start())
		})
	}
}

func TestConfiguredGroupsKeepEndpointsAndBuiltins(t *testing.T) {
	input := BoxOptions{
		BasePath: t.TempDir(),
		Options: O.Options{
			Outbounds: []O.Outbound{{Tag: "selector", Type: C.TypeSelector, Options: O.SelectorOutboundOptions{
				Outbounds: []string{"missing", "direct", "endpoint"}, Default: "endpoint",
			}}},
			Endpoints: []O.Endpoint{{Tag: "endpoint", Type: C.TypeWireGuard, Options: &O.WireGuardEndpointOptions{}}},
		},
	}
	options, err := buildOptions(input)
	require.NoError(t, err)
	out := findOutbound(options.Outbounds, "selector")
	require.NotNil(t, out)
	group := out.Options.(*O.SelectorOutboundOptions)
	assert.Equal(t, []string{"direct", "endpoint"}, group.Outbounds)
	assert.Equal(t, "endpoint", group.Default)
	assert.Equal(t, []string{"missing", "direct", "endpoint"}, input.Options.Outbounds[0].Options.(O.SelectorOutboundOptions).Outbounds)
}

func TestConfiguredGroupsDropCollapsedSmartRoutingGroup(t *testing.T) {
	options, err := buildOptions(BoxOptions{
		BasePath: t.TempDir(),
		Options: O.Options{Outbounds: []O.Outbound{
			{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
			{Tag: "selector", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
				Outbounds: []string{"sr-mixed", "working"}, Default: "sr-mixed",
			}},
		}},
		SmartRouting: lcommon.SmartRoutingRules{{Category: "mixed", Outbounds: []string{"missing", "direct"}}},
	})
	require.NoError(t, err)
	assert.Nil(t, findOutbound(options.Outbounds, "sr-mixed"))
	out := findOutbound(options.Outbounds, "selector")
	require.NotNil(t, out)
	group := out.Options.(*O.SelectorOutboundOptions)
	assert.Equal(t, []string{"working"}, group.Outbounds)
	assert.Empty(t, group.Default)
}

func configuredGroupMembers(t *testing.T, out O.Outbound) []string {
	t.Helper()
	switch opts := out.Options.(type) {
	case *O.SelectorOutboundOptions:
		return opts.Outbounds
	case *O.URLTestOutboundOptions:
		return opts.Outbounds
	case *lbO.MutableSelectorOutboundOptions:
		return opts.Outbounds
	case *lbO.MutableURLTestOutboundOptions:
		return opts.Outbounds
	case *lbO.MutableAutoSelectOutboundOptions:
		return opts.Outbounds
	default:
		t.Fatalf("unexpected group options %T", opts)
		return nil
	}
}
