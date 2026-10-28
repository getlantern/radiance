//go:build !novpn

package backend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func splitTestBackend(t *testing.T) (*LocalBackend, string) {
	t.Helper()
	dir := t.TempDir()
	settings.Reset()
	require.NoError(t, settings.InitSettings(dir))
	t.Cleanup(settings.Reset)
	manager, err := vpn.NewSplitTunnelHandler(dir, log.NoOpLogger())
	require.NoError(t, err)
	return &LocalBackend{splitTunnelMgr: manager, vpnClient: vpn.NewVPNClient(dir, log.NoOpLogger(), nil)}, dir
}

func TestSplitTunnelSettingsAreProjected(t *testing.T) {
	r, dir := splitTestBackend(t)
	require.NoError(t, settings.Patch(settings.Settings{
		settings.SplitTunnelKey: false, settings.SplitTunnelPolicyKey: "exclude",
	}))
	require.NoError(t, r.AddSplitTunnelItems(vpn.SplitTunnelFilter{
		DomainSuffix: []string{"example.com"}, ProcessPath: []string{"missing.exe"},
	}))
	require.NoError(t, r.PatchSettings(settings.Settings{
		settings.SplitTunnelKey: true, settings.SplitTunnelPolicyKey: "include",
	}))
	want := vpn.SplitTunnelState{Enabled: true, Policy: vpn.SplitTunnelPolicyInclude}
	assert.Equal(t, want, r.SplitTunnelState())
	assert.Equal(t, true, r.Settings()[settings.SplitTunnelKey])
	assert.Equal(t, "include", r.Settings()[settings.SplitTunnelPolicyKey])
	assert.False(t, settings.GetBool(settings.SplitTunnelKey))
	assert.Equal(t, "exclude", settings.GetString(settings.SplitTunnelPolicyKey))

	loaded, err := vpn.NewSplitTunnelHandler(dir, log.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, loaded.ImportLegacyPolicy(vpn.SplitTunnelPolicyExclude))
	assert.Equal(t, want, loaded.State())

	require.NoError(t, r.AddSplitTunnelItems(vpn.SplitTunnelFilter{PackageName: []string{"new.app"}}))
	require.NoError(t, r.PatchSettings(settings.Settings{settings.SplitTunnelPolicyKey: "include"}))
	assert.Equal(t, []string{"new.app"}, r.SplitTunnelFilters().PackageName)
	require.NoError(t, r.PatchSettings(settings.Settings{settings.SplitTunnelKey: false}))
	assert.Equal(t, []string{"new.app"}, r.SplitTunnelFilters().PackageName)
	assert.Equal(t, vpn.SplitTunnelPolicyInclude, r.SplitTunnelState().Policy)
}

func TestSplitTunnelSettingsRejectInvalidPatchBeforeMutation(t *testing.T) {
	r, _ := splitTestBackend(t)
	before := r.SplitTunnelState()
	for _, updates := range []settings.Settings{
		{settings.SplitTunnelKey: "true", settings.LocaleKey: "fa"},
		{settings.SplitTunnelKey: true, settings.SplitTunnelPolicyKey: "invalid", settings.LocaleKey: "fa"},
		{settings.SplitTunnelKey: true, settings.SplitTunnelPolicyKey: true},
	} {
		require.Error(t, r.PatchSettings(updates))
		assert.Equal(t, before, r.SplitTunnelState())
		assert.Empty(t, settings.GetString(settings.LocaleKey))
	}
}

func TestSplitTunnelSaveFailurePreservesStateAndSettings(t *testing.T) {
	r, dir := splitTestBackend(t)
	require.NoError(t, r.AddSplitTunnelItems(vpn.SplitTunnelFilter{PackageName: []string{"app"}}))
	before := r.SplitTunnelState()
	path := filepath.Join(dir, internal.SplitTunnelFileName)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	require.Error(t, r.PatchSettings(settings.Settings{
		settings.SplitTunnelKey: true, settings.SplitTunnelPolicyKey: "include",
	}))
	assert.Equal(t, before, r.SplitTunnelState())
	assert.False(t, settings.GetBool(settings.SplitTunnelKey))
	assert.Empty(t, settings.GetString(settings.SplitTunnelPolicyKey))
}

func TestSplitTunnelMutationDoesNotOverlapConnect(t *testing.T) {
	r, _ := splitTestBackend(t)
	r.connectMu.Lock()
	err := r.SwitchSplitTunnelPolicy(vpn.SplitTunnelPolicyInclude)
	r.connectMu.Unlock()
	require.ErrorIs(t, err, ErrConnectInProgress)
	assert.Equal(t, vpn.SplitTunnelPolicyExclude, r.SplitTunnelState().Policy)

	r.splitTunnelMu.Lock()
	err = r.ClearSplitTunnelApps()
	r.splitTunnelMu.Unlock()
	require.ErrorIs(t, err, errSplitTunnelUpdateInProgress)
}

func TestNativeRestartConnectPreservesSelection(t *testing.T) {
	r, dir := splitTestBackend(t)
	var err error
	r.srvManager, err = servers.NewManager(dir, log.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, r.srvManager.AddServers(servers.ServerList{
		Servers: []*servers.Server{{Tag: "pinned", Type: "shadowsocks"}},
	}, false))
	r.persistSelection("pinned")
	assert.Equal(t, "pinned", r.connectSelection(vpn.AutoSelectTag, vpn.Restarting))
	assert.Equal(t, "pinned", r.connectSelection("", vpn.Restarting))
	assert.Equal(t, "explicit", r.connectSelection("explicit", vpn.Restarting))
	assert.Equal(t, vpn.AutoSelectTag, r.connectSelection(vpn.AutoSelectTag, vpn.Disconnected))
	r.persistSelection(vpn.AutoSelectTag)
	assert.Equal(t, vpn.AutoSelectTag, r.connectSelection(vpn.AutoSelectTag, vpn.Restarting))
	r.persistSelection("pinned")
	_, err = r.srvManager.RemoveServers([]string{"pinned"})
	require.NoError(t, err)
	assert.Equal(t, vpn.AutoSelectTag, r.connectSelection(vpn.AutoSelectTag, vpn.Restarting))
}

func TestConnectRefusesUnpersistedSplitTunnelMigration(t *testing.T) {
	r, dir := splitTestBackend(t)
	path := filepath.Join(dir, internal.SplitTunnelFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"version":3,"rules":[{"type":"logical","mode":"or","rules":[
		{"type":"logical","mode":"and","rules":[{"domain":"disable.rule"},{"domain":"disable.rule","invert":true}]}
	]}]}`), 0o600))
	var err error
	r.splitTunnelMgr, err = vpn.NewSplitTunnelHandler(dir, log.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, settings.Set(settings.SplitTunnelPolicyKey, "include"))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	require.ErrorContains(t, r.ConnectVPN(t.Context(), vpn.AutoSelectTag), "prepare split-tunnel rules")
	assert.Equal(t, vpn.Disconnected, r.VPNStatus())
}
