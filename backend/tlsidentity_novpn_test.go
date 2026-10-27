//go:build novpn

package backend

import (
	"fmt"
	"slices"
	"testing"

	C "github.com/getlantern/common"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/servers"
)

func TestManagedTLSSelectionSurvivesLiveConfig(t *testing.T) {
	t.Setenv("RADIANCE_COUNTRY", "US")
	t.Setenv("RADIANCE_SOCKS_ADDRESS", "127.0.0.1:0")
	settings.Reset()
	t.Cleanup(settings.Reset)
	dataDir := t.TempDir()
	require.NoError(t, settings.InitSettings(dataDir))
	require.NoError(t, settings.Set(settings.DataPathKey, dataDir))

	outbound := func(tag string) option.Outbound {
		return option.Outbound{Tag: tag, Type: "http", Options: &option.HTTPOutboundOptions{
			ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
			OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{
				TLS: &option.OutboundTLSOptions{Enabled: true, Insecure: true},
			},
		}}
	}
	cfg := cachedConfig()
	cfg.Options.Outbounds = []option.Outbound{outbound("cached-tls")}
	writeCachedConfig(t, dataDir, cfg)
	r := newCachedConfigBackend(t, dataDir)
	t.Cleanup(func() { assert.NoError(t, r.vpnClient.Disconnect()) })
	require.True(t, r.applyCurrentConfig())
	require.NoError(t, r.ConnectVPN(t.Context(), "cached-tls"))

	selected, available, err := r.SelectedServer()
	require.NoError(t, err)
	require.True(t, available)
	require.NotNil(t, selected)
	assert.Equal(t, "cached-tls", selected.Tag)

	cfg.Options.Outbounds = []option.Outbound{outbound("fresh-tls")}
	r.applyConfig(cfg, true)
	for _, tag := range []string{"fresh-tls", "cached-tls"} {
		require.NoError(t, r.vpnClient.SelectServer(tag))
		r.persistSelection(tag)
		selected, available, err = r.SelectedServer()
		require.NoError(t, err)
		require.True(t, available)
		require.NotNil(t, selected)
		assert.Equal(t, tag, selected.Tag)
		server, found := r.GetServerByTag(tag)
		require.True(t, found)
		assert.False(t, server.Options.(option.Outbound).Options.(*option.HTTPOutboundOptions).TLS.Insecure)
	}
	assert.True(t, cfg.Options.Outbounds[0].Options.(*option.HTTPOutboundOptions).TLS.Insecure)

	for _, suffix := range []string{"first", "second"} {
		proxy := outbound("dependent-" + suffix)
		helper := option.Outbound{Tag: "helper-" + suffix, Type: "direct", Options: &option.DirectOutboundOptions{}}
		proxy.Options.(*option.HTTPOutboundOptions).Detour = helper.Tag
		cfg.Options.Outbounds = []option.Outbound{proxy, helper}
		cfg.NonSelectableOutbounds = []string{helper.Tag}
		r.applyConfig(cfg, true)
		for _, tag := range []string{proxy.Tag, "dependent-first"} {
			require.NoError(t, r.vpnClient.SelectServer(tag))
			current, err := r.vpnClient.CurrentSelectedServer()
			require.NoError(t, err)
			assert.Equal(t, tag, current)
		}
		_, managed := r.GetServerByTag(helper.Tag)
		assert.False(t, managed)
	}
}

func TestSmartRoutingSelectionSurvivesLiveConfig(t *testing.T) {
	t.Setenv("RADIANCE_COUNTRY", "US")
	t.Setenv("RADIANCE_SOCKS_ADDRESS", "127.0.0.1:0")
	settings.Reset()
	t.Cleanup(settings.Reset)
	dataDir := t.TempDir()
	require.NoError(t, settings.InitSettings(dataDir))
	require.NoError(t, settings.Set(settings.DataPathKey, dataDir))
	require.NoError(t, settings.Set(settings.SmartRoutingKey, true))
	cfg := smartRoutingConfig()
	writeCachedConfig(t, dataDir, cfg)
	r := newCachedConfigBackend(t, dataDir)
	t.Cleanup(func() { assert.NoError(t, r.vpnClient.Disconnect()) })
	require.True(t, r.applyCurrentConfig())
	require.NoError(t, r.ConnectVPN(t.Context(), "via-group"))

	for range 2 {
		r.applyConfig(cfg, true)
		for _, tag := range []string{"via-group", "via-detour"} {
			require.NoError(t, r.vpnClient.SelectServer(tag))
			r.persistSelection(tag)
			selected, available, err := r.SelectedServer()
			require.NoError(t, err)
			require.True(t, available)
			require.NotNil(t, selected)
			assert.Equal(t, tag, selected.Tag)
			current, err := r.vpnClient.CurrentSelectedServer()
			require.NoError(t, err)
			assert.Equal(t, tag, current)
		}
	}
}

func TestConfigChangesInfrastructureSelectionRole(t *testing.T) {
	t.Setenv("RADIANCE_COUNTRY", "US")
	t.Setenv("RADIANCE_SOCKS_ADDRESS", "127.0.0.1:0")
	_, cfg := retentionTestBackend(t)
	cfg.Options.Outbounds = []option.Outbound{
		{Tag: "changing", Type: "direct", Options: &option.DirectOutboundOptions{}},
		retentionTestServer("working", "changing").Options.(option.Outbound),
	}
	cfg.NonSelectableOutbounds = []string{"changing"}
	writeCachedConfig(t, settings.GetString(settings.DataPathKey), cfg)
	r := newCachedConfigBackend(t, settings.GetString(settings.DataPathKey))
	require.True(t, r.applyCurrentConfig())
	t.Cleanup(func() { assert.NoError(t, r.vpnClient.Disconnect()) })
	require.NoError(t, r.ConnectVPN(t.Context(), "working"))
	for _, helper := range []bool{false, true, false} {
		if helper {
			cfg.NonSelectableOutbounds = []string{"changing"}
		} else {
			cfg.NonSelectableOutbounds = nil
		}
		require.NoError(t, r.updateServers(cfg, true))
		_, managed := r.GetServerByTag("changing")
		assert.Equal(t, !helper, managed)
		infrastructure, err := r.vpnClient.NonSelectableServers()
		require.NoError(t, err)
		assert.Equal(t, helper, slices.Contains(infrastructure.Tags(), "changing"))
		if helper {
			require.NoError(t, r.vpnClient.SelectServer("changing"))
			selected, err := r.vpnClient.CurrentSelectedServer()
			require.NoError(t, err)
			assert.NotEqual(t, "changing", selected)
			assert.Nil(t, settings.Get(settings.SelectedServerKey))
		} else {
			require.NoError(t, r.vpnClient.SelectServer("changing"))
			r.persistSelection("changing")
		}
	}
}

func TestSelectedGroupRetainsInstalledSmartRoutingDependency(t *testing.T) {
	t.Setenv("RADIANCE_COUNTRY", "US")
	t.Setenv("RADIANCE_SOCKS_ADDRESS", "127.0.0.1:0")
	_, cfg := retentionTestBackend(t)
	require.NoError(t, settings.Set(settings.SmartRoutingKey, true))
	cfg.SmartRouting = C.SmartRoutingRules{{Category: "old", Outbounds: []string{"working"}}}
	cfg.Options.Outbounds = []option.Outbound{
		{Tag: "working", Type: "direct", Options: &option.DirectOutboundOptions{}},
		{Tag: "selected", Type: "selector", Options: &option.SelectorOutboundOptions{Outbounds: []string{"sr-old"}}},
	}
	writeCachedConfig(t, settings.GetString(settings.DataPathKey), cfg)
	r := newCachedConfigBackend(t, settings.GetString(settings.DataPathKey))
	require.True(t, r.applyCurrentConfig())
	t.Cleanup(func() { assert.NoError(t, r.vpnClient.Disconnect()) })
	require.NoError(t, r.ConnectVPN(t.Context(), "selected"))
	cfg.Options.Outbounds = nil
	cfg.SmartRouting = nil
	for i := range maxRetainedLanternServers {
		cfg.Options.Outbounds = append(cfg.Options.Outbounds, retentionTestServer(fmt.Sprintf("new-%d", i), "").Options.(option.Outbound))
	}
	for range 2 {
		require.NoError(t, r.updateServers(cfg, true))
		for _, tag := range []string{"selected", "working"} {
			_, found := r.GetServerByTag(tag)
			assert.True(t, found, tag)
		}
		require.NoError(t, r.vpnClient.SelectServer("selected"))
		selected, err := r.vpnClient.CurrentSelectedServer()
		require.NoError(t, err)
		assert.Equal(t, "selected", selected)
		var pin servers.Server
		require.NoError(t, settings.GetStruct(settings.SelectedServerKey, &pin))
		assert.Equal(t, "selected", pin.Tag)
	}
}
