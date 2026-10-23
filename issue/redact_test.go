package issue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/log"
)

// Issue reports upload logs, the crash log, config.json, servers.json and
// debug-box-options.json, all of which can name helper- outbounds.
func TestIssueArchiveRedactsHelperTags(t *testing.T) {
	const tag = "helper-0123456789ab"
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lantern.log"), []byte("selected "+tag+"\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lantern-crash.log"), []byte("panic in outbound/unbounded["+tag+"]\n"), 0644))
	servers := filepath.Join(dir, "servers.json")
	require.NoError(t, os.WriteFile(servers, []byte(`{"outbounds":[{"tag":"`+tag+`","type":"unbounded"}]}`), 0644))

	result, err := buildIssueArchive(log.NoOpLogger(), dir, []string{servers}, 1024*1024)
	require.NoError(t, err)
	entries := readZipEntries(t, result)
	require.Len(t, entries, 3)
	for _, e := range entries {
		assert.NotContains(t, e.content, tag, e.name)
		assert.Contains(t, e.content, "helper", e.name)
	}
}
