package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"
)

func TestApplyConfigDropsInsecureServersBeforeEviction(t *testing.T) {
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
			writeCachedConfig(t, dataDir, cfg)
			r := newCachedConfigBackend(t, dataDir)

			retained := make([]*servers.Server, maxRetainedLanternServers)
			for i := range retained {
				out := cfg.Options.Outbounds[0]
				out.Tag = fmt.Sprintf("retained-%d", i)
				retained[i] = &servers.Server{
					Tag: out.Tag, Type: out.Type, IsLantern: true, Options: out,
					SelectionHistory: &servers.SelectionHistory{HardDemoted: i == 0},
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
			_, found = r.GetServerByTag("insecure-out")
			assert.False(t, found)
			_, found = r.GetServerByTag("unbounded-out")
			assert.False(t, found)
			persisted, err := os.ReadFile(filepath.Join(dataDir, internal.ServersFileName))
			require.NoError(t, err)
			assert.NotContains(t, string(persisted), "insecure-out")
			assert.NotContains(t, string(persisted), "unbounded-out")
			for i, srv := range retained {
				_, found := r.GetServerByTag(srv.Tag)
				assert.Equal(t, !fresh || i != 0, found, "retention of %s", srv.Tag)
			}
			if fresh {
				assert.Len(t, r.srvManager.AllServers(), maxRetainedLanternServers)
				assert.Nil(t, settings.Get(settings.SelectedServerKey))
				assert.True(t, settings.GetBool(settings.AutoConnectKey))
			} else {
				assert.Len(t, r.srvManager.AllServers(), maxRetainedLanternServers+1)
				var selected servers.Server
				require.NoError(t, settings.GetStruct(settings.SelectedServerKey, &selected))
				assert.Equal(t, retained[0].Tag, selected.Tag)
				assert.False(t, settings.GetBool(settings.AutoConnectKey))
			}
		})
	}
}
