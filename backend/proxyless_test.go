package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	singjson "github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
)

func TestProxylessOn(t *testing.T) {
	settings.Reset()
	t.Cleanup(settings.Reset)
	require.NoError(t, settings.InitSettings(t.TempDir()))

	flagOn := &config.Config{Features: map[string]bool{proxylessFeature: true}}
	flagOff := &config.Config{Features: map[string]bool{}}

	assert.True(t, settings.ProxylessEnabled(), "the setting is on until the user turns it off")
	assert.True(t, proxylessOn(flagOn), "flag on and setting on by default")
	assert.False(t, proxylessOn(flagOff), "the server flag is the kill switch")
	assert.False(t, proxylessOn(nil), "no config, no proxyless")

	require.NoError(t, settings.Set(settings.ProxylessKey, false))
	assert.False(t, settings.ProxylessEnabled())
	assert.False(t, proxylessOn(flagOn), "the user's setting turns it off even with the flag on")

	require.NoError(t, settings.Set(settings.ProxylessKey, true))
	assert.True(t, proxylessOn(flagOn))
}

// TestGetBoxOptionsProxyless checks that only smart routing builds the
// proxyless hedge: full tunnel sends everything through the proxy.
func TestGetBoxOptionsProxyless(t *testing.T) {
	dataDir := t.TempDir()
	cfg := cachedConfig()
	cfg.Features = map[string]bool{proxylessFeature: true}
	buf, err := singjson.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, internal.ConfigFileName), buf, 0o600))
	settings.Reset()
	t.Cleanup(settings.Reset)
	require.NoError(t, settings.InitSettings(dataDir))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srvMgr, err := servers.NewManager(dataDir, log.NoOpLogger())
	require.NoError(t, err)
	r := &LocalBackend{
		ctx:         ctx,
		confHandler: config.NewConfigHandler(ctx, config.Options{DataPath: dataDir, Logger: log.NoOpLogger()}),
		srvManager:  srvMgr,
	}

	require.NoError(t, settings.Set(settings.SmartRoutingKey, false))
	assert.Nil(t, r.getBoxOptions().Proxyless, "full tunnel never builds the proxyless hedge")

	require.NoError(t, settings.Set(settings.SmartRoutingKey, true))
	p := r.getBoxOptions().Proxyless
	require.NotNil(t, p, "smart routing builds the proxyless hedge")
	assert.True(t, p.Enabled)

	require.NoError(t, settings.Set(settings.ProxylessKey, false))
	p = r.getBoxOptions().Proxyless
	require.NotNil(t, p, "the setting switches the hedge; it is still built so it can be switched on live")
	assert.False(t, p.Enabled)
}
