package backend

import (
	"context"
	"fmt"
	"testing"

	C "github.com/getlantern/common"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

func budgetTestServers(prefix string, count int) servers.ServerList {
	cfg := cachedConfig()
	template := cfg.Options.Outbounds[0]
	cfg.Options.Outbounds = nil
	for i := 0; i < count; i++ {
		out := template
		out.Tag = fmt.Sprintf("%s-%02d", prefix, i)
		cfg.Options.Outbounds = append(cfg.Options.Outbounds, out)
	}
	return serverListFromConfig(cfg)
}

func TestUpdateServersBoundsExistingAndIncomingBatches(t *testing.T) {
	dir := t.TempDir()
	manager, err := servers.NewManager(dir, log.NoOpLogger())
	require.NoError(t, err)
	existing := budgetTestServers("existing", 60)
	private := budgetTestServers("private", 1).Servers[0]
	private.IsLantern = false
	existing.Servers = append(existing.Servers, private)
	require.NoError(t, manager.AddServers(existing, false))
	r := &LocalBackend{ctx: context.Background(), srvManager: manager, vpnClient: vpn.NewVPNClient(dir, log.NoOpLogger(), nil)}
	for batch, count := range []int{0, 10, 10, 10, 60} {
		require.NoError(t, r.updateServers(budgetTestServers(fmt.Sprintf("batch-%d", batch), count)))
		all := manager.AllServers()
		require.Len(t, all, 13, "12 Lantern servers plus the user-added server")
		_, found := manager.GetServerByTag(private.Tag)
		require.True(t, found)
	}
}

func TestColdStartOmitsEvictedConfigRoutes(t *testing.T) {
	cfg := &config.Config{NonSelectableOutbounds: []string{"infra"}, OutboundLocations: C.OutboundLocations{}}
	for i := 0; i < 60; i++ {
		tag := fmt.Sprintf("route-%02d", i)
		if i%2 == 0 {
			cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{Tag: tag, Type: "shadowsocks"})
		} else {
			cfg.Options.Endpoints = append(cfg.Options.Endpoints, option.Endpoint{Tag: tag, Type: "wireguard"})
		}
	}
	cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{Tag: "infra", Type: "direct"})
	var managed []*servers.Server
	for i := 0; i < 12; i++ {
		managed = append(managed, &servers.Server{Tag: fmt.Sprintf("route-%02d", i), IsLantern: true})
	}
	options := cfg.Options
	filterUnmanagedConfigOptions(&options, cfg, managed)
	require.Len(t, options.Outbounds, 7)
	require.Len(t, options.Endpoints, 6)
	require.Len(t, cfg.Options.Outbounds, 31, "filter must not mutate the config snapshot")
	require.Len(t, cfg.Options.Endpoints, 30)
	require.Equal(t, "route-58", cfg.Options.Outbounds[29].Tag)
	require.Equal(t, "route-59", cfg.Options.Endpoints[29].Tag)
	require.Equal(t, "infra", options.Outbounds[6].Tag)
}

func TestColdStartBoundsConfigBeforeManagerLoads(t *testing.T) {
	cfg := cachedConfig()
	template := cfg.Options.Outbounds[0]
	cfg.Options.Outbounds = nil
	for i := 0; i < 60; i++ {
		out := template
		out.Tag = fmt.Sprintf("route-%02d", i)
		cfg.Options.Outbounds = append(cfg.Options.Outbounds, out)
	}
	options := cfg.Options
	filterUnmanagedConfigOptions(&options, cfg, nil)
	require.Len(t, options.Outbounds, 12)
	require.Equal(t, "route-00", options.Outbounds[0].Tag)
	require.Len(t, cfg.Options.Outbounds, 60)
}
