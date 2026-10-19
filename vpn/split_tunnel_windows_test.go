//go:build windows && !novpn

package vpn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	rlog "github.com/getlantern/radiance/log"
)

func TestSplitTunnelWindowsProcessPathIdentity(t *testing.T) {
	path, short := windowsProcessPathFixture(t)
	st, err := NewSplitTunnelHandler(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, st.AddItem(TypeProcessPath, short))
	require.NoError(t, st.AddItems(SplitTunnelFilter{ProcessPath: []string{
		strings.ToUpper(path), filepath.ToSlash(path),
	}}))
	assert.Equal(t, []string{strings.ToLower(path)}, st.Filters().ProcessPath)
	require.NoError(t, st.SetEnabled(true))

	rule, _ := splitTunnelMatcher(t, st)
	assert.True(t, rule.Match(&adapter.InboundContext{
		ProcessInfo: &adapter.ConnectionOwner{ProcessPath: strings.ToUpper(path)},
	}))
	assert.False(t, rule.Match(&adapter.InboundContext{
		ProcessInfo: &adapter.ConnectionOwner{ProcessPath: filepath.Join(filepath.Dir(path), "Launcher.exe")},
	}))
	assert.False(t, rule.Match(&adapter.InboundContext{
		ProcessInfo: &adapter.ConnectionOwner{ProcessPath: filepath.Join(filepath.Dir(path), "Other", filepath.Base(path))},
	}))

	require.NoError(t, st.RemoveItems(SplitTunnelFilter{ProcessPath: []string{strings.ToUpper(short)}}))
	assert.Empty(t, st.Filters().ProcessPath)
	require.NoError(t, st.AddItems(SplitTunnelFilter{ProcessPath: []string{short}}))
	require.NoError(t, st.RemoveItem(TypeProcessPath, filepath.ToSlash(strings.ToUpper(path))))
	assert.Empty(t, st.Filters().ProcessPath)
}

func TestSplitTunnelWindowsMigratesSavedProcessPaths(t *testing.T) {
	path, short := windowsProcessPathFixture(t)
	dataDir := t.TempDir()
	st, err := NewSplitTunnelHandler(dataDir, rlog.NoOpLogger())
	require.NoError(t, err)
	st.ruleMap[TypeProcessPath].ProcessPath = []string{short, strings.ToUpper(path)}
	st.ruleMap[TypeProcessPathRegex].ProcessPathRegex = []string{`C:\\Preserve\\.*`}
	st.ruleMap[TypeDomain].Domain = []string{"example.com"}
	require.NoError(t, st.SetEnabled(true))
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))

	loaded, err := NewSplitTunnelHandler(dataDir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, []string{strings.ToLower(path)}, loaded.Filters().ProcessPath)
	assert.Equal(t, []string{`C:\\Preserve\\.*`}, loaded.Filters().ProcessPathRegex)
	assert.Equal(t, []string{"example.com"}, loaded.Filters().Domain)
	assert.True(t, loaded.IsEnabled())
	assert.Equal(t, SplitTunnelPolicyInclude, loaded.Policy())

	require.NoError(t, os.Remove(path))
	reloaded, err := NewSplitTunnelHandler(dataDir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, loaded.Filters(), reloaded.Filters(), "migration must persist before the executable disappears")
	require.NoError(t, reloaded.RemoveItem(TypeProcessPath, strings.ToUpper(path)))
	assert.Empty(t, reloaded.Filters().ProcessPath)
}

func TestSplitTunnelWindowsMissingProcessPathRemainsRemovable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Missing Game.exe")
	st, err := NewSplitTunnelHandler(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, st.AddItem(TypeProcessPath, path))
	assert.Equal(t, []string{strings.ToLower(path)}, st.Filters().ProcessPath)
	require.NoError(t, st.RemoveItem(TypeProcessPath, strings.ToUpper(path)))
	assert.Empty(t, st.Filters().ProcessPath)
}

func windowsProcessPathFixture(t *testing.T) (string, string) {
	t.Helper()
	input, err := windows.UTF16PtrFromString(`\\?\` + t.TempDir())
	require.NoError(t, err)
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(input, &buffer[0], uint32(len(buffer)))
	require.NoError(t, err)
	require.Positive(t, n)
	require.Less(t, int(n), len(buffer))
	dir := strings.TrimPrefix(windows.UTF16ToString(buffer[:n]), `\\?\`)
	path := filepath.Join(dir, "Launcher Child Executable.exe")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	input, err = windows.UTF16PtrFromString(`\\?\` + path)
	require.NoError(t, err)
	n, err = windows.GetShortPathName(input, &buffer[0], uint32(len(buffer)))
	require.NoError(t, err)
	require.Positive(t, n)
	require.Less(t, int(n), len(buffer))
	return path, strings.TrimPrefix(windows.UTF16ToString(buffer[:n]), `\\?\`)
}
