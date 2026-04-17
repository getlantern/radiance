package vpn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreDownloadRuleSets_RewritesRemoteToLocal(t *testing.T) {
	body := []byte("rule-set-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	dataPath := t.TempDir()
	opts := &O.Options{Route: &O.RouteOptions{RuleSet: []O.RuleSet{{
		Type:          C.RuleSetTypeRemote,
		Tag:           "geosite-cn",
		Format:        C.RuleSetFormatBinary,
		RemoteOptions: O.RemoteRuleSet{URL: srv.URL + "/cn.srs"},
	}}}}

	preDownloadRuleSets(context.Background(), opts, dataPath)

	rs := opts.Route.RuleSet[0]
	require.Equal(t, C.RuleSetTypeLocal, rs.Type)
	assert.Equal(t, "geosite-cn", rs.Tag)
	got, err := os.ReadFile(rs.LocalOptions.Path)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestPreDownloadRuleSets_UsesFreshCacheWithoutHTTP(t *testing.T) {
	dataPath := t.TempDir()
	cached := []byte("cached-bytes")
	url := "https://example.invalid/cn.srs" // would fail if dialed
	path := ruleSetCachePath(filepath.Join(dataPath, ruleSetsCacheDir), "geosite-cn", url)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, cached, 0644))

	opts := &O.Options{Route: &O.RouteOptions{RuleSet: []O.RuleSet{{
		Type:          C.RuleSetTypeRemote,
		Tag:           "geosite-cn",
		Format:        C.RuleSetFormatBinary,
		RemoteOptions: O.RemoteRuleSet{URL: url, UpdateInterval: badoption.Duration(time.Hour)},
	}}}}

	preDownloadRuleSets(context.Background(), opts, dataPath)

	rs := opts.Route.RuleSet[0]
	require.Equal(t, C.RuleSetTypeLocal, rs.Type)
	assert.Equal(t, path, rs.LocalOptions.Path)
	got, err := os.ReadFile(rs.LocalOptions.Path)
	require.NoError(t, err)
	assert.Equal(t, cached, got, "cache was overwritten, download should have been skipped")
}

func TestPreDownloadRuleSets_FallsBackToStaleCacheOnFailure(t *testing.T) {
	dataPath := t.TempDir()
	stale := []byte("stale-but-usable")
	url := "https://example.invalid/cn.srs"
	path := ruleSetCachePath(filepath.Join(dataPath, ruleSetsCacheDir), "geosite-cn", url)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, stale, 0644))
	// Backdate mtime well beyond the TTL.
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	opts := &O.Options{Route: &O.RouteOptions{RuleSet: []O.RuleSet{{
		Type:          C.RuleSetTypeRemote,
		Tag:           "geosite-cn",
		Format:        C.RuleSetFormatBinary,
		RemoteOptions: O.RemoteRuleSet{URL: url, UpdateInterval: badoption.Duration(time.Hour)},
	}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	preDownloadRuleSets(ctx, opts, dataPath)

	rs := opts.Route.RuleSet[0]
	require.Equal(t, C.RuleSetTypeLocal, rs.Type, "stale cache should still let us switch to local")
	assert.Equal(t, path, rs.LocalOptions.Path)
}

func TestPreDownloadRuleSets_LeavesLocalEntriesAlone(t *testing.T) {
	opts := &O.Options{Route: &O.RouteOptions{RuleSet: []O.RuleSet{{
		Type:         C.RuleSetTypeLocal,
		Tag:          "split-tunnel",
		Format:       C.RuleSetFormatSource,
		LocalOptions: O.LocalRuleSet{Path: "/some/path.json"},
	}}}}
	preDownloadRuleSets(context.Background(), opts, t.TempDir())
	rs := opts.Route.RuleSet[0]
	assert.Equal(t, C.RuleSetTypeLocal, rs.Type)
	assert.Equal(t, "/some/path.json", rs.LocalOptions.Path)
}
