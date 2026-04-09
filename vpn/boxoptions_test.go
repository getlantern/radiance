package vpn

import (
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
					"rule_set": "openai",
					"outbound": "sr-openai"
				}
			],
			"rule_set": [
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
	adBlockJSON := `
	{
		"route": {
			"rules": [
				{
					"rule_set": [
						"adblock-1",
						"adblock-2"
					],
					"action": "reject"
				}
			],
			"rule_set": [
				{
				  "type": "remote",
				  "tag": "adblock-1",
				  "url": "https://ruleset.com/adblock-1.srs",
				  "download_detour": "direct",
				  "update_interval": "24h0m0s"
				},
				{
				  "type": "remote",
				  "tag": "adblock-2",
				  "url": "https://ruleset.com/adblock-2.srs",
				  "download_detour": "direct",
				  "update_interval": "24h0m0s"
				}
			]
		}
	}
	`
	wantSmartRoutingOpts, err := json.UnmarshalExtendedContext[O.Options](box.BaseContext(), []byte(smartRouteJSON))
	require.NoError(t, err)
	wantAdBlockOpts, err := json.UnmarshalExtendedContext[O.Options](box.BaseContext(), []byte(adBlockJSON))
	require.NoError(t, err)

	cfg := testConfig(t)
	boxOptions := BoxOptions{
		BasePath: t.TempDir(),
		Options:  cfg.Options,
	}
	t.Run("with smart routing", func(t *testing.T) {
		boxOptions.SmartRouting = cfg.SmartRouting
		options, err := buildOptions(boxOptions)
		require.NoError(t, err)
		// check rules, rulesets, and outbounds are correctly built into options
		assert.True(t, contains(t, options.Route.Rules, wantSmartRoutingOpts.Route.Rules[0]), "missing smart routing rule")
		assert.True(t, contains(t, options.Route.RuleSet, wantSmartRoutingOpts.Route.RuleSet[0]), "missing smart routing ruleset")
		assert.True(t, contains(t, options.Outbounds, wantSmartRoutingOpts.Outbounds[0]), "missing smart routing outbound")
	})
	t.Run("with smart routing and missing outbounds", func(t *testing.T) {
		boxOptions.SmartRouting = cfg.SmartRouting
		cfg.SmartRouting[0].Outbounds = nil
		options, err := buildOptions(boxOptions)
		require.NoError(t, err)
		// check rules, rulesets, and outbounds are not built into options
		assert.False(t, contains(t, options.Route.Rules, wantSmartRoutingOpts.Route.Rules[0]), "missing smart routing rule")
		assert.False(t, contains(t, options.Route.RuleSet, wantSmartRoutingOpts.Route.RuleSet[0]), "missing smart routing ruleset")
		assert.False(t, contains(t, options.Outbounds, wantSmartRoutingOpts.Outbounds[0]), "missing smart routing outbound")
	})
	t.Run("with ad block", func(t *testing.T) {
		boxOptions.AdBlock = cfg.AdBlock
		options, err := buildOptions(boxOptions)
		require.NoError(t, err)
		// check reject rule and rulesets are correctly built into options
		for _, rs := range wantAdBlockOpts.Route.RuleSet {
			assert.True(t, contains(t, options.Route.RuleSet, rs), "missing ad block ruleset")
		}

		adRule := wantAdBlockOpts.Route.Rules[0]
		assert.True(t, contains(t, options.Route.Rules, adRule), "missing ad block rule")
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
