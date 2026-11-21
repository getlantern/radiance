package backend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

func TestIsPrivateServer(t *testing.T) {
	t.Setenv("RADIANCE_COUNTRY", "US")
	dataDir := t.TempDir()
	writeCachedConfig(t, dataDir, cachedConfig())
	r := newCachedConfigBackend(t, dataDir)
	r.applyCurrentConfig()
	require.NoError(t, r.AddServers(servers.ServerList{Servers: []*servers.Server{{
		Tag: "user-out", Type: "shadowsocks", Options: option.Outbound{
			Tag: "user-out", Type: "shadowsocks", Options: &option.ShadowsocksOutboundOptions{},
		},
	}}}))

	assert.True(t, r.isPrivateServer("user-out"))
	assert.False(t, r.isPrivateServer("cached-out"), "a Lantern server")
	assert.False(t, r.isPrivateServer("unknown"), "a tag the manager doesn't hold")
}

func TestUsageStatsWithoutStore(t *testing.T) {
	r := &LocalBackend{}
	assert.Zero(t, r.UsageStats())
	assert.NoError(t, r.ResetUsageStats())

	path := filepath.Join(t.TempDir(), "usage-stats.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"lifetime":{"direct":{"up":1,"down":2}}}`), 0o600))
	r.UseUsageStatsForTest(vpn.NewUsageStats(path))
	assert.Equal(t, int64(2), r.UsageStats().Lifetime.Direct.Down)
	require.NoError(t, r.ResetUsageStats())
	assert.Zero(t, r.UsageStats().Lifetime)
}
