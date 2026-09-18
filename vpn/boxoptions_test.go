package vpn

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/config"
)

func TestBuildOptions(t *testing.T) {
	options, tags := testBoxOptions(t)
	tests := []struct {
		name        string
		boxOptions  BoxOptions
		shouldError bool
	}{
		{
			name: "success",
			boxOptions: BoxOptions{
				BasePath: t.TempDir(),
				Options:  options,
			},
		},
		{
			name: "no servers available",
			boxOptions: BoxOptions{
				BasePath: t.TempDir(),
			},
			shouldError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := buildOptions(tt.boxOptions)
			if tt.shouldError {
				require.Error(t, err, "expected error but got none")
				return
			}
			require.NoError(t, err)

			urlTest := urlTestOutbound(AutoSelectTag, tags, nil)
			assert.Contains(t, opts.Outbounds, urlTest, "options should contain auto-select URL test outbound")
			selector := selectorOutbound(ManualSelectTag, tags)
			assert.Contains(t, opts.Outbounds, selector, "options should contain manual-select selector outbound")
		})
	}
}

func TestBuildOptions_Rulesets(t *testing.T) {
	smartRouteJSON := `
	{
		"outbounds": [
			{
				"type": "urltest",
				"tag": "sr-openai",
				"outbounds": ["http1-out", "socks1-out"],
				"url": "https://google.com/generate_204",
				"interval": "3m0s",
				"idle_timeout": "15m"
			}
		],
		"route": {
			"rules": [
				{
					"rule_set": "sr-direct",
					"outbound": "direct"
				},
				{
					"rule_set": "openai",
					"outbound": "sr-openai"
				}
			],
			"rule_set": [
				{
				  "type": "remote",
				  "tag": "sr-direct",
				  "url": "https://ruleset.com/direct.srs",
				  "download_detour": "direct",
				  "update_interval": "24h0m0s"
				},
				{
				  "type": "remote",
				  "tag": "openai",
				  "url": "https://ruleset.com/openai.srs",
				  "download_detour": "direct",
				  "update_interval": "24h0m0s"
				}
			]
		}
	}
	`
	wantSmartRoutingOpts, err := json.UnmarshalExtendedContext[O.Options](box.BaseContext(), []byte(smartRouteJSON))
	require.NoError(t, err)

	t.Run("with smart routing", func(t *testing.T) {
		cfg := testConfig(t)
		boxOptions := BoxOptions{
			BasePath:     t.TempDir(),
			Options:      cfg.Options,
			SmartRouting: cfg.SmartRouting,
		}
		options, err := buildOptions(boxOptions)
		require.NoError(t, err)
		// check rules, rulesets, and outbounds are correctly built into options
		assert.Subset(t, options.Route.Rules, wantSmartRoutingOpts.Route.Rules, "missing smart routing rule")
		assert.Subset(t, options.Route.RuleSet, wantSmartRoutingOpts.Route.RuleSet, "missing smart routing ruleset")
		assert.Subset(t, options.Outbounds, wantSmartRoutingOpts.Outbounds, "missing smart routing outbound")
	})
	t.Run("with smart routing and missing outbounds", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.SmartRouting[1].Outbounds = nil
		boxOptions := BoxOptions{
			BasePath:     t.TempDir(),
			Options:      cfg.Options,
			SmartRouting: cfg.SmartRouting,
		}
		options, err := buildOptions(boxOptions)
		require.NoError(t, err)
		// sr-direct rule and ruleset should still be present (category still has outbounds)
		assert.Contains(t, options.Route.Rules, wantSmartRoutingOpts.Route.Rules[0], "missing sr-direct rule")
		assert.Contains(t, options.Route.RuleSet, wantSmartRoutingOpts.Route.RuleSet[0], "missing sr-direct ruleset")
		// openai rule/ruleset and sr-openai outbound should be dropped (outbounds were nilled)
		assert.NotContains(t, options.Route.Rules, wantSmartRoutingOpts.Route.Rules[1], "unexpected openai rule")
		assert.NotContains(t, options.Route.RuleSet, wantSmartRoutingOpts.Route.RuleSet[1], "unexpected openai ruleset")
		assert.NotContains(t, options.Outbounds, wantSmartRoutingOpts.Outbounds[0], "unexpected sr-openai outbound")
	})
	t.Run("with ad block", func(t *testing.T) {
		cfg := testConfig(t)
		boxOptions := BoxOptions{
			BasePath: t.TempDir(),
			Options:  cfg.Options,
			AdBlock:  cfg.AdBlock,
		}
		wantRule, wantRulesets := cfg.AdBlock.ToOptions()
		// buildOptions round-trips the options through JSON, which lets sing-box
		// fill in the reject action's default method; ToOptions leaves it unset.
		wantRule.DefaultOptions.RejectOptions.Method = C.RuleActionRejectMethodDefault
		options, err := buildOptions(boxOptions)
		require.NoError(t, err)
		// check reject rule and rulesets are correctly built into options
		assert.Contains(t, options.Route.Rules, wantRule, "missing ad block rule")
		assert.Subset(t, options.Route.RuleSet, wantRulesets, "missing ad block ruleset")
	})
}

func TestBuildOptions_BanditURLOverrides(t *testing.T) {
	cfg := testConfig(t)
	overrides := map[string]string{
		cfg.Options.Outbounds[0].Tag: "https://example.com/callback?token=abc",
	}
	boxOptions := BoxOptions{
		BasePath:           t.TempDir(),
		Options:            cfg.Options,
		BanditURLOverrides: overrides,
	}
	opts, err := buildOptions(boxOptions)
	require.NoError(t, err)

	out := findOutbound(opts.Outbounds, AutoSelectTag)
	require.NotNil(t, out, "missing auto-select outbound")

	require.IsType(t, &lbO.MutableAutoSelectOutboundOptions{}, out.Options, "auto outbound options should be MutableAutoSelectOutboundOptions")
	autoOpts := out.Options.(*lbO.MutableAutoSelectOutboundOptions)
	assert.Equal(t, overrides, autoOpts.URLOverrides, "URLOverrides should be wired from config")
}

func TestBuildOptions_WATERDirOverride(t *testing.T) {
	cfg := testConfig(t)
	basePath := t.TempDir()
	waterOutbound := O.Outbound{
		Tag:  "water-test",
		Type: lbC.TypeWATER,
		Options: &lbO.WATEROutboundOptions{
			Dir: "/tmp/stale",
		},
	}
	cfg.Options.Outbounds = append(cfg.Options.Outbounds, waterOutbound)
	boxOptions := BoxOptions{
		BasePath: basePath,
		Options:  cfg.Options,
	}

	opts, err := buildOptions(boxOptions)
	require.NoError(t, err)

	out := findOutbound(opts.Outbounds, "water-test")
	require.NotNil(t, out, "water-test outbound missing from built options")
	waterOpts, ok := out.Options.(*lbO.WATEROutboundOptions)
	require.True(t, ok, "expected *WATEROutboundOptions")
	assert.Equal(t, filepath.Join(basePath, "water"), waterOpts.Dir,
		"buildOptions must override Dir to the app-managed water directory")

	// Original struct must not be mutated.
	assert.Equal(t, "/tmp/stale", waterOutbound.Options.(*lbO.WATEROutboundOptions).Dir,
		"buildOptions must not mutate the caller's WATEROutboundOptions")
}

func contains[S ~[]E, E any](t *testing.T, s S, e E) bool {
	for _, v := range s {
		if optsEqual(t, v, e) {
			return true
		}
	}
	return false
}

func optsEqual[T any](t *testing.T, want, got T) bool {
	wantBuf, err := json.Marshal(want)
	require.NoError(t, err, "marshal wanted options")
	gotBuf, err := json.Marshal(got)
	require.NoError(t, err, "marshal got options")
	return string(wantBuf) == string(gotBuf)
}

func filterOutbounds(opts O.Options, typ string) ([]string, []O.Outbound) {
	var outbounds []O.Outbound
	var tags []string
	for _, o := range opts.Outbounds {
		if o.Type == typ {
			outbounds = append(outbounds, o)
			tags = append(tags, o.Tag)
		}
	}
	return tags, outbounds
}

func findOutbound(outs []O.Outbound, tag string) *O.Outbound {
	idx := slices.IndexFunc(outs, func(o O.Outbound) bool {
		return o.Tag == tag
	})
	if idx == -1 {
		return nil
	}
	return &outs[idx]
}

func testConfig(t *testing.T) config.Config {
	buf, err := os.ReadFile("testdata/config.json")
	require.NoError(t, err, "read test config file")

	cfg, err := json.UnmarshalExtendedContext[config.Config](box.BaseContext(), buf)
	require.NoError(t, err, "unmarshal test config")
	return cfg
}

func testBoxOptions(t *testing.T) (O.Options, []string) {
	cfg := testConfig(t)
	var tags []string
	for _, o := range cfg.Options.Outbounds {
		tags = append(tags, o.Tag)
	}
	for _, ep := range cfg.Options.Endpoints {
		tags = append(tags, ep.Tag)
	}
	return cfg.Options, tags
}

// TestBuildOptions_RejectsQUICAfterDirectRules pins the placement of the QUIC
// reject: it must follow the split-tunnel and smart-routing rules (so a
// direct-routed domain keeps its QUIC) and precede the selector rules (so
// QUIC bound for the proxy is rejected).
func TestBuildOptions_RejectsIPv6WhenCaptured(t *testing.T) {
	cfg := testConfig(t)
	opts, err := buildOptions(BoxOptions{
		BasePath:     t.TempDir(),
		Options:      cfg.Options,
		SmartRouting: cfg.SmartRouting,
		AdBlock:      cfg.AdBlock,
	})
	require.NoError(t, err)

	isIPv6Reject := func(r O.Rule) bool {
		o := r.DefaultOptions
		return o.RuleAction.Action == "reject" && slices.Contains(o.RawDefaultRule.IPCIDR, "::/0")
	}
	rejectIdx := slices.IndexFunc(opts.Route.Rules, isIPv6Reject)

	if !tunHasIPv6(opts) {
		require.Equal(t, -1, rejectIdx, "no ::/0 reject expected when the TUN has no IPv6 address")
		return
	}

	isSplitTunnel := func(r O.Rule) bool {
		return slices.Contains(r.DefaultOptions.RawDefaultRule.RuleSet, splitTunnelTag)
	}
	isSelector := func(r O.Rule) bool {
		mode := r.DefaultOptions.RawDefaultRule.ClashMode
		return mode == AutoSelectTag || mode == ManualSelectTag
	}
	splitIdx := slices.IndexFunc(opts.Route.Rules, isSplitTunnel)
	selectorIdx := slices.IndexFunc(opts.Route.Rules, isSelector)

	require.NotEqual(t, -1, splitIdx, "expected split-tunnel rule in built options")
	require.NotEqual(t, -1, selectorIdx, "expected at least one selector mode rule in built options")
	assert.Greater(t, rejectIdx, splitIdx, "IPv6 reject must come after split-tunnel so split-direct v6 keeps flowing")
	assert.Less(t, rejectIdx, selectorIdx, "IPv6 reject must come before selector rules so proxied v6 is rejected")
}

func TestRejectIPv6Rule(t *testing.T) {
	r := rejectIPv6Rule()
	assert.Equal(t, "reject", r.DefaultOptions.RuleAction.Action)
	assert.Equal(t, []string{"::/0"}, []string(r.DefaultOptions.RawDefaultRule.IPCIDR))
	assert.NotEqual(t, C.RuleActionRejectMethodDrop, r.DefaultOptions.RuleAction.RejectOptions.Method,
		"must not use the drop method — a silent blackhole stalls instead of failing over to IPv4")
	assert.True(t, r.DefaultOptions.RuleAction.RejectOptions.NoDrop,
		"NoDrop must be set so the reject returns unreachable instead of dropping")
}

func TestTunHasIPv6(t *testing.T) {
	tun := func(addrs ...string) O.Options {
		prefixes := make([]netip.Prefix, len(addrs))
		for i, a := range addrs {
			prefixes[i] = netip.MustParsePrefix(a)
		}
		return O.Options{Inbounds: []O.Inbound{{
			Type:    "tun",
			Options: &O.TunInboundOptions{Address: prefixes},
		}}}
	}
	assert.True(t, tunHasIPv6(tun("10.10.1.1/30", "fdfe:dcba:9876::1/126")),
		"TUN with a v6 address captures IPv6")
	assert.False(t, tunHasIPv6(tun("10.10.1.1/30")), "v4-only TUN does not capture IPv6")
	assert.False(t, tunHasIPv6(O.Options{}), "no inbounds means no IPv6 capture")
}
