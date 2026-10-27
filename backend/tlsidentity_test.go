package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	C "github.com/getlantern/common"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

func smartRoutingConfig() *config.Config {
	cfg := cachedConfig()
	cfg.SmartRouting = C.SmartRoutingRules{{Category: "live", Outbounds: []string{"cached-out"}}}
	cfg.Options.Outbounds = append(cfg.Options.Outbounds,
		option.Outbound{Tag: "via-group", Type: "selector", Options: &option.SelectorOutboundOptions{Outbounds: []string{"sr-live"}}},
		option.Outbound{Tag: "via-detour", Type: "http", Options: &option.HTTPOutboundOptions{
			ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
			DialerOptions: option.DialerOptions{Detour: "sr-live"},
		}},
	)
	return cfg
}

func TestApplyConfigNormalizesSmartRoutingDependencies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		empty   bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled"},
		{name: "empty", enabled: true, empty: true},
	} {
		for _, fresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fresh=%v", tc.name, fresh), func(t *testing.T) {
				settings.Reset()
				t.Cleanup(settings.Reset)
				dataDir := t.TempDir()
				require.NoError(t, settings.InitSettings(dataDir))
				require.NoError(t, settings.Set(settings.SmartRoutingKey, tc.enabled))
				cfg := smartRoutingConfig()
				if tc.empty {
					cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{
						Tag: "denied", Type: lbC.TypeReflex, Options: &lbO.ReflexOutboundOptions{},
					})
					cfg.SmartRouting[0].Outbounds = []string{"denied"}
				}
				writeCachedConfig(t, dataDir, cfg)
				r := newCachedConfigBackend(t, dataDir)
				if fresh {
					r.applyConfig(cfg, true)
				} else {
					require.True(t, r.applyCurrentConfig())
				}
				for _, tag := range []string{"via-group", "via-detour"} {
					_, found := r.GetServerByTag(tag)
					assert.Equal(t, tc.enabled && !tc.empty, found, tag)
				}
				_, found := r.GetServerByTag("cached-out")
				assert.True(t, found)
			})
		}
	}
}

func TestApplyConfigNormalizesTLSBeforeEviction(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached", true: "fresh"}[fresh], func(t *testing.T) {
			t.Setenv("RADIANCE_COUNTRY", "US")
			settings.Reset()
			t.Cleanup(settings.Reset)
			dataDir := t.TempDir()
			require.NoError(t, settings.InitSettings(dataDir))

			cfg := cachedConfig()
			cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{
				Tag: "insecure-out", Type: "http", Options: &option.HTTPOutboundOptions{
					ServerOptions: option.ServerOptions{Server: "example.com", ServerPort: 443},
					OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{
						TLS: &option.OutboundTLSOptions{Enabled: true, Insecure: true},
					},
				},
			})
			cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{
				Tag: "unbounded-out", Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{
					InsecureDoNotVerifyClientCert: true, InsecureDoNotVerifyDiscoveryCert: true,
				},
			})
			cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{
				Tag: "reflex-out", Type: lbC.TypeReflex, Options: &lbO.ReflexOutboundOptions{
					CertPEM: "client-certificate", KeyPEM: "private-key",
				},
			})
			writeCachedConfig(t, dataDir, cfg)
			r := newCachedConfigBackend(t, dataDir)

			retained := make([]*servers.Server, maxRetainedLanternServers)
			for i := range retained {
				out := cfg.Options.Outbounds[0]
				out.Tag = fmt.Sprintf("retained-%d", i)
				retained[i] = &servers.Server{
					Tag: out.Tag, Type: out.Type, IsLantern: true, Options: out,
					SelectionHistory: &servers.SelectionHistory{HardDemoted: i == 0, UpdatedAt: time.Unix(int64(i), 0)},
				}
			}
			require.NoError(t, r.srvManager.AddServers(servers.ServerList{Servers: retained}, false))
			r.persistSelection(retained[0].Tag)

			if fresh {
				r.applyConfig(cfg, true)
			} else {
				require.True(t, r.applyCurrentConfig())
			}

			_, found := r.GetServerByTag("cached-out")
			assert.True(t, found, "one rejected server must not block valid servers")
			normalized, found := r.GetServerByTag("insecure-out")
			require.True(t, found)
			assert.False(t, normalized.Options.(option.Outbound).Options.(*option.HTTPOutboundOptions).TLS.Insecure)
			assert.True(t, cfg.Options.Outbounds[1].Options.(*option.HTTPOutboundOptions).TLS.Insecure)
			_, found = r.GetServerByTag("unbounded-out")
			assert.False(t, found)
			_, found = r.GetServerByTag("reflex-out")
			assert.False(t, found)
			persisted, err := os.ReadFile(filepath.Join(dataDir, internal.ServersFileName))
			require.NoError(t, err)
			assert.Contains(t, string(persisted), "insecure-out")
			assert.NotContains(t, string(persisted), "unbounded-out")
			assert.NotContains(t, string(persisted), "reflex-out")
			for i, srv := range retained {
				_, found := r.GetServerByTag(srv.Tag)
				assert.Equal(t, !fresh || i > 1, found, "retention of %s", srv.Tag)
			}
			if fresh {
				assert.Len(t, r.srvManager.AllServers(), maxRetainedLanternServers)
				assert.Nil(t, settings.Get(settings.SelectedServerKey))
				assert.True(t, settings.GetBool(settings.AutoConnectKey))
			} else {
				assert.Len(t, r.srvManager.AllServers(), maxRetainedLanternServers+2)
				var selected servers.Server
				require.NoError(t, settings.GetStruct(settings.SelectedServerKey, &selected))
				assert.Equal(t, retained[0].Tag, selected.Tag)
				assert.False(t, settings.GetBool(settings.AutoConnectKey))
			}
		})
	}
}

func TestRejectedDependenciesDoNotEvictRetainedServers(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprint("fresh=", fresh), func(t *testing.T) {
			settings.Reset()
			t.Cleanup(settings.Reset)
			dataDir := t.TempDir()
			require.NoError(t, settings.InitSettings(dataDir))
			cfg := cachedConfig()
			writeCachedConfig(t, dataDir, cfg)
			r := newCachedConfigBackend(t, dataDir)
			var retained []*servers.Server
			for i := 0; i < maxRetainedLanternServers; i++ {
				out := cfg.Options.Outbounds[0]
				out.Tag = fmt.Sprintf("retained-%d", i)
				retained = append(retained, &servers.Server{Tag: out.Tag, Type: out.Type, Options: out, IsLantern: true})
			}
			require.NoError(t, r.srvManager.AddServers(servers.ServerList{Servers: retained}, false))
			cfg.Options.Outbounds = []option.Outbound{{Tag: "denied", Type: lbC.TypeReflex, Options: &lbO.ReflexOutboundOptions{}}}
			for i := 0; i < maxRetainedLanternServers; i++ {
				cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{
					Tag: fmt.Sprintf("dependent-%d", i), Type: "http", Options: &option.HTTPOutboundOptions{
						ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
						DialerOptions: option.DialerOptions{Detour: "denied"},
					},
				})
			}
			r.applyConfig(cfg, fresh)
			assert.ElementsMatch(t, (servers.ServerList{Servers: retained}).Tags(), (servers.ServerList{Servers: r.srvManager.AllServers()}).Tags())
			persisted, err := os.ReadFile(filepath.Join(dataDir, internal.ServersFileName))
			require.NoError(t, err)
			assert.NotContains(t, string(persisted), "dependent-")
			assert.NotContains(t, string(persisted), "denied")
		})
	}
}

func TestConfigPreservesAvailableDependenciesBeforeEviction(t *testing.T) {
	settings.Reset()
	t.Cleanup(settings.Reset)
	dataDir := t.TempDir()
	require.NoError(t, settings.InitSettings(dataDir))
	require.NoError(t, settings.Set(settings.SmartRoutingKey, true))
	cfg := cachedConfig()
	writeCachedConfig(t, dataDir, cfg)
	r := newCachedConfigBackend(t, dataDir)
	var retained []*servers.Server
	for i := 0; i < maxRetainedLanternServers; i++ {
		out := cfg.Options.Outbounds[0]
		out.Tag = fmt.Sprintf("retained-%d", i)
		if i == 1 {
			out = option.Outbound{Tag: out.Tag, Type: "http", Options: &option.HTTPOutboundOptions{
				ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
				DialerOptions: option.DialerOptions{Detour: "retained-0"},
			}}
		}
		retained = append(retained, &servers.Server{Tag: out.Tag, Type: out.Type, Options: out, IsLantern: true,
			SelectionHistory: &servers.SelectionHistory{UpdatedAt: time.Unix(int64(i), 0)},
		})
	}
	require.NoError(t, r.srvManager.AddServers(servers.ServerList{Servers: retained}, false))
	cfg.Options.Outbounds = []option.Outbound{{Tag: "helper", Type: "direct", Options: &option.DirectOutboundOptions{}}}
	cfg.Options.Endpoints = []option.Endpoint{{Tag: "endpoint", Type: "wireguard", Options: &option.WireGuardEndpointOptions{}}}
	cfg.NonSelectableOutbounds = []string{"helper", "endpoint"}
	cfg.SmartRouting = C.SmartRoutingRules{{Category: "live", Outbounds: []string{"retained-1"}}}
	for _, dependency := range []string{"sr-live", "direct", "helper", "endpoint"} {
		cfg.Options.Outbounds = append(cfg.Options.Outbounds, option.Outbound{Tag: "via-" + dependency, Type: "http", Options: &option.HTTPOutboundOptions{
			ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			DialerOptions: option.DialerOptions{Detour: dependency},
		}})
	}
	r.applyConfig(cfg, true)
	for _, tag := range []string{"via-sr-live", "via-direct", "via-helper", "via-endpoint", "retained-0", "retained-1"} {
		_, found := r.GetServerByTag(tag)
		assert.True(t, found, tag)
	}
	for _, tag := range []string{"helper", "endpoint", "retained-2"} {
		_, found := r.GetServerByTag(tag)
		assert.False(t, found, tag)
	}
	assert.Len(t, r.srvManager.AllServers(), maxRetainedLanternServers)
}

func retentionTestServer(tag, detour string) *servers.Server {
	return &servers.Server{Tag: tag, Type: "http", IsLantern: true, Options: option.Outbound{
		Tag: tag, Type: "http", Options: &option.HTTPOutboundOptions{
			ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			DialerOptions: option.DialerOptions{Detour: detour},
		},
	}}
}

func retentionTestBackend(t *testing.T) (*LocalBackend, *config.Config) {
	t.Helper()
	settings.Reset()
	t.Cleanup(settings.Reset)
	dataDir := t.TempDir()
	require.NoError(t, settings.InitSettings(dataDir))
	require.NoError(t, settings.Set(settings.DataPathKey, dataDir))
	cfg := cachedConfig()
	writeCachedConfig(t, dataDir, cfg)
	return newCachedConfigBackend(t, dataDir), cfg
}

func TestHardDemotedDependenciesDoNotEvictHealthyServers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		viaParent bool
		reoffer   bool
	}{
		{name: "retained-dependency", viaParent: true},
		{name: "reoffered-demotion", reoffer: true},
		{name: "reoffered-transitive-demotion", viaParent: true, reoffer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, cfg := retentionTestBackend(t)
			var healthy []*servers.Server
			for i := range maxRetainedLanternServers {
				healthy = append(healthy, retentionTestServer(fmt.Sprintf("healthy-%d", i), ""))
			}
			blocked := retentionTestServer("blocked", "")
			blocked.SelectionHistory = &servers.SelectionHistory{HardDemoted: true}
			parent := retentionTestServer("parent", blocked.Tag)
			retained := append(slices.Clone(healthy), blocked, parent)
			require.NoError(t, r.srvManager.AddServers(servers.ServerList{Servers: retained}, false))
			dependency := blocked.Tag
			if tc.viaParent {
				dependency = parent.Tag
			}
			cfg.Options.Outbounds = nil
			if tc.reoffer {
				cfg.Options.Outbounds = append(cfg.Options.Outbounds, blocked.Options.(option.Outbound))
			}
			for i := range maxRetainedLanternServers {
				cfg.Options.Outbounds = append(cfg.Options.Outbounds, retentionTestServer(fmt.Sprintf("incoming-%d", i), dependency).Options.(option.Outbound))
			}
			require.NoError(t, r.updateServers(cfg, true))
			assert.ElementsMatch(t, (servers.ServerList{Servers: healthy}).Tags(), (servers.ServerList{Servers: r.srvManager.AllServers()}).Tags())
		})
	}
}

func TestSelectedGroupRetainsTransitiveDependencies(t *testing.T) {
	for _, lantern := range []bool{true, false} {
		t.Run(fmt.Sprintf("lantern=%v", lantern), func(t *testing.T) {
			r, cfg := retentionTestBackend(t)
			leaf := retentionTestServer("leaf", "")
			child := retentionTestServer("child", leaf.Tag)
			group := &servers.Server{Tag: "selected", Type: "selector", IsLantern: lantern, Options: option.Outbound{
				Tag: "selected", Type: "selector", Options: &option.SelectorOutboundOptions{Outbounds: []string{child.Tag}},
			}}
			retained := []*servers.Server{leaf, child, group}
			for i := range maxRetainedLanternServers - 3 {
				retained = append(retained, retentionTestServer(fmt.Sprintf("retained-%d", i), ""))
			}
			require.NoError(t, r.srvManager.AddServers(servers.ServerList{Servers: retained}, false))
			r.persistSelection(group.Tag)
			cfg.Options.Outbounds = nil
			for i := range maxRetainedLanternServers {
				cfg.Options.Outbounds = append(cfg.Options.Outbounds, retentionTestServer(fmt.Sprintf("incoming-%d", i), "").Options.(option.Outbound))
			}
			require.NoError(t, r.updateServers(cfg, true))
			for _, tag := range []string{group.Tag, child.Tag, leaf.Tag} {
				_, found := r.GetServerByTag(tag)
				assert.True(t, found, tag)
			}
			var selected servers.Server
			require.NoError(t, settings.GetStruct(settings.SelectedServerKey, &selected))
			assert.Equal(t, group.Tag, selected.Tag)
			normalized, _ := vpn.NormalizeServers(servers.ServerList{Servers: r.srvManager.AllServers()}, nil, vpn.ReservedTags()...)
			assert.Contains(t, normalized.Tags(), group.Tag)
		})
	}
}

func TestConfigRestoresExistingGroupMembers(t *testing.T) {
	r, cfg := retentionTestBackend(t)
	groupOptions := &option.SelectorOutboundOptions{Outbounds: []string{"a", "b"}}
	cfg.Options.Outbounds = []option.Outbound{
		{Tag: "group", Type: "selector", Options: groupOptions},
		retentionTestServer("a", "").Options.(option.Outbound),
	}
	require.NoError(t, r.updateServers(cfg, true))
	group, found := r.GetServerByTag("group")
	require.True(t, found)
	assert.Equal(t, []string{"a"}, group.Options.(option.Outbound).Options.(*option.SelectorOutboundOptions).Outbounds)
	group.Credentials = &servers.ServerCredentials{AccessToken: "kept"}
	group.Location.CountryCode = "US"
	require.NoError(t, r.srvManager.AddServers(servers.ServerList{Servers: []*servers.Server{group}}, true))
	history := servers.SelectionHistory{UpdatedAt: time.Unix(123, 0)}
	require.NoError(t, r.srvManager.UpdateSelectionHistory(map[string]servers.SelectionHistory{group.Tag: history}))
	cfg.Options.Outbounds = append(cfg.Options.Outbounds, retentionTestServer("b", "").Options.(option.Outbound))
	require.NoError(t, r.updateServers(cfg, true))
	restored, found := r.GetServerByTag(group.Tag)
	require.True(t, found)
	assert.Equal(t, []string{"a", "b"}, restored.Options.(option.Outbound).Options.(*option.SelectorOutboundOptions).Outbounds)
	assert.Equal(t, group.Credentials, restored.Credentials)
	assert.Equal(t, group.Location, restored.Location)
	assert.Equal(t, history, *restored.SelectionHistory)
	assert.Equal(t, []string{"a", "b"}, groupOptions.Outbounds)
}

func TestCapacityEvictionPrunesUnselectedGroupDependencies(t *testing.T) {
	r, cfg := retentionTestBackend(t)
	var retained []*servers.Server
	for i := range maxRetainedLanternServers - 1 {
		srv := retentionTestServer(fmt.Sprintf("retained-%d", i), "")
		srv.SelectionHistory = &servers.SelectionHistory{UpdatedAt: time.Unix(int64(i), 0)}
		retained = append(retained, srv)
	}
	group := &servers.Server{Tag: "group", Type: "selector", IsLantern: true, Options: option.Outbound{
		Tag: "group", Type: "selector", Options: &option.SelectorOutboundOptions{Outbounds: []string{retained[0].Tag}},
	}, SelectionHistory: &servers.SelectionHistory{UpdatedAt: time.Unix(100, 0)}}
	retained = append(retained, group)
	require.NoError(t, r.srvManager.AddServers(servers.ServerList{Servers: retained}, false))
	cfg.Options.Outbounds = []option.Outbound{retentionTestServer("incoming", "").Options.(option.Outbound)}
	require.NoError(t, r.updateServers(cfg, true))
	_, found := r.GetServerByTag(retained[0].Tag)
	assert.False(t, found)
	_, found = r.GetServerByTag(group.Tag)
	assert.False(t, found)
}
