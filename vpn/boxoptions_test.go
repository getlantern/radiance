package vpn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	LC "github.com/getlantern/common"
	sbx "github.com/getlantern/sing-box-extensions"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/servers"
)

func TestBuildOptions(t *testing.T) {
	httpTags, httpOutbounds := getTestOutbounds(t, constant.TypeHTTP)
	socksTags, socksOutbounds := getTestOutbounds(t, constant.TypeSOCKS)
	cfg := LC.ConfigResponse{
		Options: option.Options{
			Outbounds: httpOutbounds,
		},
	}
	svrs := servers.Servers{
		servers.SGUser: servers.Options{
			Outbounds: socksOutbounds,
		},
	}
	httpAllOutbounds := append(
		httpOutbounds,
		urlTestOutbound(autoLanternTag, httpTags),
		selectorOutbound(servers.SGLantern, append([]string{autoLanternTag}, httpTags...)),
	)
	socksAllOutbounds := append(
		socksOutbounds,
		urlTestOutbound(autoUserTag, socksTags),
		selectorOutbound(servers.SGUser, append([]string{autoUserTag}, socksTags...)),
	)
	collectTags := func(outbounds []option.Outbound) []string {
		tags := make([]string, 0, len(outbounds))
		for _, o := range outbounds {
			tags = append(tags, o.Tag)
		}
		return tags
	}
	httpTags = collectTags(httpAllOutbounds)
	socksTags = collectTags(socksAllOutbounds)

	tests := []struct {
		name    string
		hasCfg  bool
		hasSvrs bool
		want    []string
		assert  func(*testing.T, []option.Outbound)
	}{
		{
			name:   "config without user servers",
			hasCfg: true,
			want: append(
				httpTags,
				selectorOutbound(servers.SGUser, []string{"block"}).Tag,
				urlTestOutbound(autoAllTag, httpTags).Tag,
			),
		},
		{
			name:    "user servers without config",
			hasSvrs: true,
			want: append(
				socksTags,
				selectorOutbound(servers.SGLantern, []string{"block"}).Tag,
				urlTestOutbound(autoAllTag, socksTags).Tag,
			),
		},
		{
			name:    "config and user servers",
			hasCfg:  true,
			hasSvrs: true,
			want: append(
				append(httpTags, socksTags...),
				urlTestOutbound(autoAllTag, append(httpTags, socksTags...)).Tag,
			),
		},
		{
			name: "neither config nor user servers",
			want: []string{
				selectorOutbound(servers.SGLantern, []string{"block"}).Tag,
				selectorOutbound(servers.SGUser, []string{"block"}).Tag,
				urlTestOutbound(autoAllTag, []string{"block"}).Tag,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir()
			if tt.hasCfg {
				testOptsToFile(t, cfg, filepath.Join(path, common.ConfigFileName))
			}
			if tt.hasSvrs {
				testOptsToFile(t, svrs, filepath.Join(path, common.ServersFileName))
			}
			opts, err := buildOptions(autoAllTag, path)
			require.NoError(t, err)

			gotOutbounds := opts.Outbounds
			gotTags := collectTags(gotOutbounds)
			assert.Subset(t, gotTags, tt.want, "options should contain expected outbounds")
		})
	}
}

func testOptsToFile[T any](t *testing.T, opts T, path string) {
	buf, err := json.Marshal(opts)
	require.NoError(t, err, "marshal options")
	require.NoError(t, os.WriteFile(path, buf, 0600), "write options to file")
}

func getTestOutbounds(t *testing.T, outType string) ([]string, []option.Outbound) {
	bOpts, _, err := testBoxOptions("testdata/boxops.json")
	require.NoError(t, err, "get test box options")
	var outbounds []option.Outbound
	var tags []string
	for _, o := range bOpts.Outbounds {
		if o.Type == outType {
			outbounds = append(outbounds, o)
			tags = append(tags, o.Tag)
		}
	}
	require.NotEmptyf(t, outbounds, "no %s outbounds found", outType)
	return tags, outbounds
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
