package vpn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	LC "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/servers"
)

func TestBuildOptions(t *testing.T) {
	testOpts, _, err := testBoxOptions("")
	require.NoError(t, err, "get test box options")
	lanternTags, lanternOuts := filterOutbounds(*testOpts, constant.TypeHTTP)
	userTags, userOuts := filterOutbounds(*testOpts, constant.TypeSOCKS)
	cfg := config.Config{
		ConfigResponse: LC.ConfigResponse{
			Options: O.Options{
				Outbounds: lanternOuts,
			},
		},
	}
	svrs := servers.Servers{
		servers.SGUser: servers.Options{
			Outbounds: userOuts,
		},
	}
	tests := []struct {
		name        string
		lanternTags []string
		userTags    []string
		shouldError bool
	}{
		{
			name:        "config without user servers",
			lanternTags: lanternTags,
		},
		{
			name:     "user servers without config",
			userTags: userTags,
		},
		{
			name:        "config and user servers",
			lanternTags: lanternTags,
			userTags:    userTags,
		},
		{
			name:        "neither config nor user servers",
			shouldError: true,
		},
	}
	hasGroupWithTags := func(t *testing.T, outs []O.Outbound, group string, tags []string) {
		out := findOutbound(outs, group)
		if !assert.NotNilf(t, out, "group %s not found", group) {
			return
		}
		switch opts := out.Options.(type) {
		case *lbO.MutableSelectorOutboundOptions:
			assert.ElementsMatchf(t, tags, opts.Outbounds, "group %s does not have correct outbounds", group)
		case *O.SelectorOutboundOptions:
			assert.ElementsMatchf(t, tags, opts.Outbounds, "group %s does not have correct outbounds", group)
		case *lbO.MutableURLTestOutboundOptions:
			assert.ElementsMatchf(t, tags, opts.Outbounds, "group %s does not have correct outbounds", group)
		case *O.URLTestOutboundOptions:
			assert.ElementsMatchf(t, tags, opts.Outbounds, "group %s does not have correct outbounds", group)
		default:
			assert.Failf(t, fmt.Sprintf("%s[%T] is not a group outbound", group, opts), "")
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir()
			if len(tt.lanternTags) > 0 {
				testOptsToFile(t, cfg, filepath.Join(path, common.ConfigFileName))
			}
			if len(tt.userTags) > 0 {
				testOptsToFile(t, svrs, filepath.Join(path, common.ServersFileName))
			}
			opts, err := buildOptions(context.Background(), path)
			if tt.shouldError {
				require.Error(t, err, "expected error but got none")
				return
			}
			require.NoError(t, err)

			gotOutbounds := opts.Outbounds
			require.NotEmpty(t, gotOutbounds, "no outbounds in built options")

			assert.NotNil(t, findOutbound(gotOutbounds, constant.TypeDirect), "direct outbound not found")
			assert.NotNil(t, findOutbound(gotOutbounds, constant.TypeBlock), "block outbound not found")

			hasGroupWithTags(t, gotOutbounds, servers.SGLantern, append(tt.lanternTags, autoLanternTag))
			hasGroupWithTags(t, gotOutbounds, servers.SGUser, append(tt.userTags, autoUserTag))

			hasGroupWithTags(t, gotOutbounds, autoLanternTag, tt.lanternTags)
			hasGroupWithTags(t, gotOutbounds, autoUserTag, tt.userTags)
			hasGroupWithTags(t, gotOutbounds, autoAllTag, []string{autoLanternTag, autoUserTag})

			assert.FileExists(t, filepath.Join(path, debugLanternBoxOptionsFilename), "debug option file must be written")
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
					"outbound": "openai"
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

	buf, err := os.ReadFile("testdata/config.json")
	require.NoError(t, err, "read test config file")

	t.Run("with smart routing", func(t *testing.T) {
		tmp := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(tmp, common.ConfigFileName), buf, 0644), "write test config file to temp dir")

		require.NoError(t, settings.InitSettings(tmp))
		t.Cleanup(settings.Reset)

		settings.Set(settings.SmartRoutingKey, true)
		options, err := buildOptions(context.Background(), tmp)
		require.NoError(t, err)
		// check rules, rulesets, and outbounds are correctly built into options
		assert.True(t, contains(t, options.Route.Rules, wantSmartRoutingOpts.Route.Rules[0]), "missing smart routing rule")
		assert.True(t, contains(t, options.Route.RuleSet, wantSmartRoutingOpts.Route.RuleSet[0]), "missing smart routing ruleset")
		assert.True(t, contains(t, options.Outbounds, wantSmartRoutingOpts.Outbounds[0]), "missing smart routing outbound")
	})
	t.Run("with ad block", func(t *testing.T) {
		tmp := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(tmp, common.ConfigFileName), buf, 0644), "write test config file to temp dir")

		require.NoError(t, settings.InitSettings(tmp))
		t.Cleanup(settings.Reset)

		settings.Set(settings.AdBlockKey, true)
		options, err := buildOptions(context.Background(), tmp)
		require.NoError(t, err)
		// check reject rule and rulesets are correctly built into options
		for _, rs := range wantAdBlockOpts.Route.RuleSet {
			assert.True(t, contains(t, options.Route.RuleSet, rs), "missing ad block ruleset")
		}

		adRule := wantAdBlockOpts.Route.Rules[0]
		assert.True(t, contains(t, options.Route.Rules, adRule), "missing ad block rule")
	})
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

func testOptsToFile[T any](t *testing.T, opts T, path string) {
	buf, err := json.Marshal(opts)
	require.NoError(t, err, "marshal options")
	require.NoError(t, os.WriteFile(path, buf, 0644), "write options to file")
}

func testBoxOptions(tmpPath string) (*O.Options, string, error) {
	content, err := os.ReadFile("testdata/boxopts.json")
	if err != nil {
		return nil, "", err
	}
	opts, err := json.UnmarshalExtendedContext[O.Options](box.BaseContext(), content)
	if err != nil {
		return nil, "", err
	}

	opts.Experimental.CacheFile.Path = filepath.Join(tmpPath, cacheFileName)
	opts.Experimental.CacheFile.CacheID = cacheID
	buf, _ := json.Marshal(opts)
	return &opts, string(buf), nil
}
