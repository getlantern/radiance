package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	C "github.com/getlantern/common"
	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

func TestApplyCurrentConfigLoadsCachedServers(t *testing.T) {
	t.Setenv("RADIANCE_COUNTRY", "US")
	dataDir := t.TempDir()
	cfg := cachedConfig()
	buf, err := singjson.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, internal.ConfigFileName), buf, 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := config.NewConfigHandler(ctx, config.Options{
		DataPath: dataDir,
		Logger:   log.NoOpLogger(),
	})
	srvMgr, err := servers.NewManager(dataDir, log.NoOpLogger())
	require.NoError(t, err)
	r := &LocalBackend{
		ctx:         ctx,
		confHandler: ch,
		srvManager:  srvMgr,
		vpnClient:   vpn.NewVPNClient(dataDir, log.NoOpLogger(), nil),
	}

	r.applyCurrentConfig()

	server, found := r.GetServerByTag("cached-out")
	require.True(t, found)
	assert.True(t, server.IsLantern)
	assert.Equal(t, "shadowsocks", server.Type)
	assert.Equal(t, "Shanghai", server.Location.City)
	assert.Equal(t, "CN", server.Location.CountryCode)
}

func cachedConfig() *config.Config {
	return &config.Config{
		Country: "CN",
		OutboundLocations: C.OutboundLocations{
			"cached-out": {
				Country:     "China",
				City:        "Shanghai",
				CountryCode: "CN",
			},
		},
		Options: option.Options{
			Outbounds: []option.Outbound{{
				Tag:  "cached-out",
				Type: "shadowsocks",
				Options: &option.ShadowsocksOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "127.0.0.1",
						ServerPort: 443,
					},
					Method:   "chacha20-ietf-poly1305",
					Password: "password",
				},
			}},
		},
	}
}

// TestNewLocalBackendToleratesInvalidOnDiskState guards the init hardening:
// invalid-but-readable on-disk state must not make NewLocalBackend fatal, so a
// user can always report an issue. Every fixture is readable, so a returned
// error can only come from parsing — a non-IO failure that must stay non-fatal.
func TestNewLocalBackendToleratesInvalidOnDiskState(t *testing.T) {
	dataDir := t.TempDir()
	logDir := t.TempDir()
	// setupDirectories honors these env vars over the Options paths; pin them so
	// the resolved data dir is exactly where the invalid files are staged.
	t.Setenv("RADIANCE_DATA_PATH", dataDir)
	t.Setenv("RADIANCE_LOG_PATH", logDir)

	invalidFiles := map[string][]byte{
		"settings.json":              []byte(`{invalid json}`),
		internal.ConfigFileName:      []byte(`{"options":{"outbounds":[{"type":"future-proto"}]}}`),
		internal.ServersFileName:     []byte(`[{"tag":"bad","type":"future-proto","outbound":{"tag":"bad","type":"future-proto"}}]`),
		internal.SplitTunnelFileName: []byte(`{"version":3,"rules":[{"unknown_field":true}]}`),
	}
	for name, content := range invalidFiles {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, name), content, 0o600))
	}

	backend, err := NewLocalBackend(context.Background(), Options{DataDir: dataDir, LogDir: logDir, LogLevel: "error"})
	require.NoError(t, err, "invalid but readable on-disk state must not make initialization fatal")
	require.NotNil(t, backend)
	t.Cleanup(backend.Close)
}

func TestExhaustionGate_AllowRateLimitsBelowGap(t *testing.T) {
	prev := defaultExhaustionRefetchGap
	defaultExhaustionRefetchGap = 50 * time.Millisecond
	t.Cleanup(func() { defaultExhaustionRefetchGap = prev })

	var g exhaustionGate
	require.True(t, g.allow(), "first allow must pass on a zero gate")
	assert.False(t, g.allow(), "second allow inside the gap must be rate-limited")
	assert.False(t, g.allow(), "third allow inside the gap must still be rate-limited")

	time.Sleep(defaultExhaustionRefetchGap + 10*time.Millisecond)
	assert.True(t, g.allow(), "allow after the gap elapses must pass again")
	assert.False(t, g.allow(), "post-recovery allow must re-arm the gate")
}

func newTestServer(tag string, isLantern, hardDemoted bool, updatedAt time.Time) *servers.Server {
	srv := &servers.Server{
		Tag:       tag,
		IsLantern: isLantern,
	}
	if hardDemoted || !updatedAt.IsZero() {
		srv.SelectionHistory = &servers.SelectionHistory{
			HardDemoted: hardDemoted,
			UpdatedAt:   updatedAt,
		}
	}
	return srv
}

func TestLanternServersToEvict(t *testing.T) {
	baseTime := time.Unix(0, 0).UTC()

	tests := []struct {
		name     string
		existing []*servers.Server
		incoming int
		limit    int
		want     []string
	}{
		{
			name: "evicts only hard-demoted lantern servers",
			existing: []*servers.Server{
				newTestServer("demoted", true, true, baseTime),
				newTestServer("working", true, false, baseTime),
				newTestServer("users-demoted", false, true, baseTime),
			},
			limit: 60,
			want:  []string{"demoted"},
		},
		{
			name: "hard-demoted lantern server is evicted regardless of incoming config",
			existing: []*servers.Server{
				newTestServer("demoted", true, true, baseTime),
			},
			incoming: 1,
			limit:    60,
			want:     []string{"demoted"},
		},
		{
			name: "under the limit nothing is evicted",
			existing: []*servers.Server{
				newTestServer("a", true, false, baseTime),
				newTestServer("b", true, false, baseTime),
			},
			incoming: 2,
			limit:    60,
		},
		{
			name: "over the limit evicts oldest working servers and keeps the newest",
			existing: []*servers.Server{
				newTestServer("old", true, false, baseTime.Add(1*time.Hour)),
				newTestServer("mid", true, false, baseTime.Add(2*time.Hour)),
				newTestServer("new", true, false, baseTime.Add(3*time.Hour)),
			},
			incoming: 1,
			limit:    3,
			want:     []string{"old"},
		},
		{
			name: "incoming at the limit evicts all existing working servers",
			existing: []*servers.Server{
				newTestServer("a", true, false, baseTime.Add(1*time.Hour)),
				newTestServer("b", true, false, baseTime.Add(2*time.Hour)),
			},
			incoming: 3,
			limit:    3,
			want:     []string{"a", "b"},
		},
		{
			name: "server with no selection history sorts oldest",
			existing: []*servers.Server{
				newTestServer("no-history", true, false, time.Time{}),
				newTestServer("probed", true, false, baseTime.Add(5*time.Hour)),
			},
			incoming: 1,
			limit:    2,
			want:     []string{"no-history"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got := lanternServersToEvict(tt.existing, tt.incoming, tt.limit)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestAppendManagedServerOptionsIncludesRetainedLanternServers(t *testing.T) {
	options := option.Options{
		Outbounds: []option.Outbound{
			{Tag: "current", Type: "shadowsocks"},
			{Tag: "retained-current", Type: "shadowsocks"},
		},
		Endpoints: []option.Endpoint{
			{Tag: "current-endpoint", Type: "wireguard"},
		},
	}
	managed := []*servers.Server{
		{
			Tag:       "retained-current",
			Type:      "hysteria2",
			IsLantern: true,
			Options:   option.Outbound{Tag: "retained-current", Type: "hysteria2"},
		},
		{
			Tag:       "server-alias-for-current",
			Type:      "hysteria2",
			IsLantern: true,
			Options:   option.Outbound{Tag: "current", Type: "hysteria2"},
		},
		{
			Tag:       "retained-missing",
			Type:      "shadowsocks",
			IsLantern: true,
			Options:   option.Outbound{Tag: "retained-missing", Type: "shadowsocks"},
		},
		{
			Tag:       "user-missing",
			Type:      "trojan",
			IsLantern: false,
			Options:   option.Outbound{Tag: "user-missing", Type: "trojan"},
		},
		{
			Tag:       "missing-option-tag",
			Type:      "shadowsocks",
			IsLantern: true,
			Options:   option.Outbound{Type: "shadowsocks"},
		},
		{
			Tag:       "retained-endpoint",
			Type:      "wireguard",
			IsLantern: true,
			Options:   option.Endpoint{Tag: "retained-endpoint", Type: "wireguard"},
		},
		{
			Tag:       "server-alias-for-current-endpoint",
			Type:      "wireguard",
			IsLantern: true,
			Options:   option.Endpoint{Tag: "current-endpoint", Type: "wireguard"},
		},
		{
			Tag:       "metadata-only",
			IsLantern: true,
		},
	}

	appendManagedServerOptions(&options, managed)

	assert.Equal(t, []string{
		"current",
		"retained-current",
		"retained-missing",
		"user-missing",
	}, outboundTags(options.Outbounds))
	assert.Equal(t, "shadowsocks", options.Outbounds[1].Type, "current config should win duplicate tags")
	assert.Equal(t, []string{
		"current-endpoint",
		"retained-endpoint",
	}, endpointTags(options.Endpoints))
}

func outboundTags(outbounds []option.Outbound) []string {
	tags := make([]string, 0, len(outbounds))
	for _, out := range outbounds {
		tags = append(tags, out.Tag)
	}
	return tags
}

func endpointTags(endpoints []option.Endpoint) []string {
	tags := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		tags = append(tags, endpoint.Tag)
	}
	return tags
}
