package vpn

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	LC "github.com/getlantern/common"
	sbx "github.com/getlantern/sing-box-extensions"
	sbxO "github.com/getlantern/sing-box-extensions/option"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/servers"
)

func TestBuildOptions(t *testing.T) {
	handlerOptions := &slog.HandlerOptions{
		Level: slog.LevelDebug - 4,
	}

	// Create a new logger with the configured handler options
	logger := slog.New(slog.NewTextHandler(os.Stderr, handlerOptions))

	// Set the default logger
	slog.SetDefault(logger)
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
	}{
		/*
			{
				name:        "config without user servers",
				lanternTags: lanternTags,
			},
			{
				name:     "user servers without config",
				userTags: userTags,
			},
		*/
		{
			name:        "config and user servers",
			lanternTags: lanternTags,
			userTags:    userTags,
		},
		/*
			{
				name: "neither config nor user servers",
			},
		*/
	}
	hasGroupWithTags := func(t *testing.T, outs []O.Outbound, group string, tags []string) {
		out := findOutbound(outs, group)
		if !assert.NotNilf(t, out, "group %s not found", group) {
			return
		}
		switch opts := out.Options.(type) {
		case *sbxO.MutableSelectorOutboundOptions:
			assert.ElementsMatchf(t, tags, opts.Outbounds, "group %s does not have correct outbounds", group)
		case *O.SelectorOutboundOptions:
			assert.ElementsMatchf(t, tags, opts.Outbounds, "group %s does not have correct outbounds", group)
		case *sbxO.MutableURLTestOutboundOptions:
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
			opts, err := buildOptions(autoAllTag, path)
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
		})
	}
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

func testBoxOptions(tmpPath string) (*option.Options, string, error) {
	content, err := os.ReadFile("testdata/boxopts.json")
	if err != nil {
		return nil, "", err
	}
	opts, err := json.UnmarshalExtendedContext[option.Options](sbx.BoxContext(), content)
	if err != nil {
		return nil, "", err
	}

	opts.Experimental.CacheFile.Path = filepath.Join(tmpPath, cacheFileName)
	opts.Experimental.CacheFile.CacheID = cacheID
	buf, _ := json.Marshal(opts)
	return &opts, string(buf), nil
}
