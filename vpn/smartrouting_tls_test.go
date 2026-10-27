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

func TestNormalizeSmartRoutingRules(t *testing.T) {
	rules := lcommon.SmartRoutingRules{
		{Category: "mixed", Outbounds: []string{" missing ", " outbound ", " endpoint ", " "}},
		{Category: "empty", Outbounds: []string{"missing"}},
		{Category: "direct", Outbounds: []string{" direct "}},
	}
	options := O.Options{
		Outbounds: []O.Outbound{{Tag: "outbound"}},
		Endpoints: []O.Endpoint{{Tag: "endpoint"}},
	}
	got := normalizeSmartRoutingRules(rules, availableOutboundTags(options, reservedTags...))
	assert.Equal(t, lcommon.SmartRoutingRules{
		{Category: "mixed", Outbounds: []string{"outbound", "endpoint"}},
		{Category: "direct", Outbounds: []string{"direct"}},
	}, got)
	assert.Equal(t, []string{" missing ", " outbound ", " endpoint ", " "}, rules[0].Outbounds)
	assert.Equal(t, []string{"missing"}, rules[1].Outbounds)
	assert.Equal(t, []string{" direct "}, rules[2].Outbounds)
}

func TestTunnelSmartRoutingDropsUnverifiableUnbounded(t *testing.T) {
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
						InsecureDoNotVerifyClientCert: true, InsecureDoNotVerifyDiscoveryCert: true,
					}},
					{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
				}},
				SmartRouting: lcommon.SmartRoutingRules{
					{Category: "mixed", Outbounds: []string{"unbounded", "working"}, RuleSets: lcommon.RuleSets{{Tag: "mixed", URL: "https://example.com/mixed.json"}}},
					{Category: "empty", Outbounds: []string{"unbounded"}, RuleSets: lcommon.RuleSets{{Tag: "empty", URL: "https://example.com/empty.json"}}},
				},
			}
			ctx := box.Context(t.Context())
			original, err := json.MarshalContext(ctx, input)
			require.NoError(t, err)
			if source == "cached" {
				input, err = json.UnmarshalExtendedContext[BoxOptions](ctx, original)
				require.NoError(t, err)
				original, err = json.MarshalContext(ctx, input)
				require.NoError(t, err)
			}
			options, err := buildOptions(input)
			require.NoError(t, err)
			group := findOutbound(options.Outbounds, "sr-mixed")
			require.NotNil(t, group)
			assert.Equal(t, []string{"working"}, group.Options.(*O.URLTestOutboundOptions).Outbounds)
			assert.Nil(t, findOutbound(options.Outbounds, "sr-empty"))
			assert.Nil(t, findOutbound(options.Outbounds, "unbounded"))
			for _, rule := range options.Route.Rules {
				assert.NotEqual(t, "sr-empty", rule.DefaultOptions.RouteOptions.Outbound)
			}
			for _, ruleSet := range options.Route.RuleSet {
				assert.NotEqual(t, "empty", ruleSet.Tag)
			}
			after, err := json.MarshalContext(ctx, input)
			require.NoError(t, err)
			assert.JSONEq(t, string(original), string(after))
			for _, out := range options.Outbounds {
				switch opts := out.Options.(type) {
				case *O.URLTestOutboundOptions:
					opts.URL = probe.URL
				case *lbO.MutableAutoSelectOutboundOptions:
					opts.URL = probe.URL
				}
			}
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
