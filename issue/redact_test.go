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

// A plan-2 direct-mode helper outbound names the volunteer's address, port
// and keys; the diagnostic copies keep only its type and a redacted tag.
func TestIssueArchiveStubsHelperOutbounds(t *testing.T) {
	const tag = "helper-0123456789ab"
	dir := t.TempDir()
	servers := filepath.Join(dir, "servers.json")
	require.NoError(t, os.WriteFile(servers, []byte(`[
 {"tag":"`+tag+`","type":"samizdat","isLantern":true,"outbound":{"type":"samizdat","tag":"`+tag+`","server":"203.0.113.77","server_port":48213,"public_key":"c2VjcmV0LWtleS1ieXRlcw"}},
 {"tag":"lantern-1","type":"samizdat","isLantern":true,"outbound":{"type":"samizdat","tag":"lantern-1","server":"198.51.100.9","server_port":443}}
]`), 0644))
	config := filepath.Join(dir, "debug-box-options.json")
	require.NoError(t, os.WriteFile(config, []byte(`{"outbounds":[
 {"type":"samizdat","tag":"`+tag+`","server":"203.0.113.77","server_port":48213,"public_key":"c2VjcmV0LWtleS1ieXRlcw"},
 {"type":"selector","tag":"manual","outbounds":["lantern-1","`+tag+`"]}],
 "endpoints":[{"type":"wireguard","tag":"`+tag+`","address":["203.0.113.77/32"]}],
 "bandit_url_overrides":{"`+tag+`":{"server":"203.0.113.77"}}}`), 0644))
	broken := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(broken, []byte(`{"outbounds":[{"tag":"`+tag+`","server":"203.0.113.77"`), 0644))

	result, err := buildIssueArchive(log.NoOpLogger(), dir, []string{servers, config, broken}, 1024*1024)
	require.NoError(t, err)
	entries := readZipEntries(t, result)
	names := map[string]string{}
	for _, e := range entries {
		names[e.name] = e.content
		for _, secret := range []string{"203.0.113.77", "48213", "c2VjcmV0LWtleS1ieXRlcw", tag} {
			assert.NotContains(t, e.content, secret, e.name)
		}
	}
	assert.NotContains(t, names, "attachments/config.json", "an unparseable config is dropped, never uploaded raw")
	require.Contains(t, names, "attachments/servers.json")
	assert.Contains(t, names["attachments/servers.json"], "198.51.100.9", "other servers are kept")
	assert.Contains(t, names["attachments/servers.json"], `"tag": "helper"`)
	assert.Contains(t, names["attachments/debug-box-options.json"], `"type": "samizdat"`)
}

// A tail read that starts mid-line, here 30 bytes before the end of the first
// line, drops that line rather than upload a fragment whose helper outbound
// was cut off and whose address was not.
func TestLogTailsDropTheLineTheyCut(t *testing.T) {
	const line = "outbound/samizdat[helper-0123456789ab]: dial tcp 203.0.113.7:443\n"

	dir := t.TempDir()
	logPath := filepath.Join(dir, "lantern-crash.log")
	require.NoError(t, os.WriteFile(logPath, []byte(line+line), 0644))
	data, err := snapshotLogFile(logPath, int64(len(line)+30)/maxLogReadFactor+1)
	require.NoError(t, err)
	assert.Equal(t, line, string(data))

	gzPath := filepath.Join(dir, "lantern.log.1.gz")
	writeGzipFile(t, gzPath, []byte(line+line))
	data, err = readGzipTail(gzPath, int64(len(line)+30))
	require.NoError(t, err)
	assert.Equal(t, line, string(data))
}
