//go:build !windows

package vpn

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/fileperm"
)

// A process with a restrictive umask, as the root network extension may run
// with, still creates the file and its lock in fileperm.File.
func TestUsageStatsSharedUnderARestrictiveUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := newTestUsageStats(path, &now)
	s.count(usageLantern, true, 10)
	s.Flush()

	for _, p := range []string{path, path + ".lock"} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, fileperm.File, info.Mode().Perm(), p)
	}
}

// The repair changes only a file with one link: another name for it keeps
// its mode.
func TestUsageStatsRepairLeavesAHardLinkedFileAlone(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "elsewhere.json")
	require.NoError(t, os.WriteFile(other, []byte("{}"), 0o600))
	require.NoError(t, os.Chmod(other, otherUsageMode()))
	path := filepath.Join(dir, "usage-stats.json")
	require.NoError(t, os.Link(other, path))

	NewUsageStats(path)
	info, err := os.Stat(other)
	require.NoError(t, err)
	assert.Equal(t, otherUsageMode(), info.Mode().Perm())
}
