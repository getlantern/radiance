package vpn

import (
	"net"
	"os"
	"slices"
	"testing"

	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
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

	require.IsType(t, &lbO.MutableURLTestOutboundOptions{}, out.Options, "auto-select outbound options should be MutableURLTestOutboundOptions")
	mutOpts := out.Options.(*lbO.MutableURLTestOutboundOptions)
	assert.Equal(t, overrides, mutOpts.URLOverrides, "URLOverrides should be wired from config")
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

func TestIsGlobalIPv6(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// Globally-routable unicast (2000::/3): want true.
		{"comcast global", "2603:8000:d0f0:5950::1", true},
		{"google global", "2607:f8b0:4006:80b::200e", true},
		{"cloudflare global", "2606:4700::1111", true},
		{"v6 documentation range (still in 2000::/3)", "2001:db8::1", true},
		{"6to4 (2002::/16, in 2000::/3)", "2002:c612:1::1", true},

		// Inside the v6 unicast space but NOT 2000::/3: want false.
		{"link-local", "fe80::1", false},
		{"ULA fc", "fc00::1", false},
		{"ULA fd", "fdfe:dcba:9876::1", false},
		{"loopback", "::1", false},
		{"unspecified", "::", false},
		{"multicast", "ff02::1", false},

		// IPv4 in any representation: want false.
		{"ipv4 private", "192.168.1.1", false},
		{"ipv4 public", "8.8.8.8", false},
		{"v4-mapped v6", "::ffff:192.168.1.1", false},
		{"v4-compatible v6 (deprecated)", "::192.168.1.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "test input %q failed to parse", tt.ip)
			assert.Equal(t, tt.want, isGlobalIPv6(ip), "isGlobalIPv6(%s)", tt.ip)
		})
	}
}

// TestBaseOpts_TunInet6Address asserts that the TUN inbound's Inet6Address
// is consistent with hasGlobalIPv6() — present when the host has a real
// global v6 address, absent when it doesn't. Pinning this behavior so
// future refactors don't accidentally drop IPv6 routing support on
// dual-stack networks or accidentally enable it on v4-only networks where
// it's known to cause regressions.
func TestBaseOpts_TunInet6Address(t *testing.T) {
	opts := baseOpts(t.TempDir())
	require.NotEmpty(t, opts.Inbounds, "expected inbounds in baseOpts output")

	var tunOpts *O.TunInboundOptions
	for _, in := range opts.Inbounds {
		if in.Type == "tun" {
			var ok bool
			tunOpts, ok = in.Options.(*O.TunInboundOptions)
			require.True(t, ok, "expected *TunInboundOptions for tun inbound")
			break
		}
	}
	require.NotNil(t, tunOpts, "expected a tun inbound")
	require.Len(t, tunOpts.Address, 1, "expected exactly one v4 TUN address")
	require.Equal(t, "10.10.1.1/30", tunOpts.Address[0].String())

	if hasGlobalIPv6() {
		require.Len(t, tunOpts.Inet6Address, 1, "expected v6 ULA on TUN when system has global v6")
		assert.Equal(t, "fdfe:dcba:9876::1/126", tunOpts.Inet6Address[0].String(),
			"v6 ULA prefix should be the ULA we picked")
	} else {
		assert.Empty(t, tunOpts.Inet6Address, "expected no v6 ULA on TUN when system has no global v6")
	}
}

func TestKernelBelow(t *testing.T) {
	tests := []struct {
		name string
		v    string
		min  string
		want bool
	}{
		{"below major", "4.19.0", "5.10", true},
		{"below minor", "5.4.0", "5.10", true},
		{"equal", "5.10.0", "5.10", false},
		{"above minor", "5.15.0", "5.10", false},
		{"above major", "6.1.0", "5.10", false},
		{"android suffix", "4.19.0-android13", "5.10", true},
		{"android suffix above", "5.15.0-android13", "5.10", false},
		{"empty version", "", "5.10", false},
		{"empty min", "5.10.0", "", false},
		{"both empty", "", "", false},
		{"invalid version", "not-a-version", "5.10", false},
		{"only major", "5", "5.10", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kernelBelow(tt.v, tt.min))
		})
	}
}
