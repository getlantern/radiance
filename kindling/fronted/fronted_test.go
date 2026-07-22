package fronted

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadBootstrapConfig_EmbeddedIsValid guards the offline-boot guarantee: a
// fresh install with no on-disk cache (and configURL blocked) must still boot,
// which means the embedded fallback has to parse.
func TestLoadBootstrapConfig_EmbeddedIsValid(t *testing.T) {
	cfg, err := loadBootstrapConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml.gz"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, cfg.Providers, "embedded fronted config must contain providers")
}

// TestLoadBootstrapConfig_FallsBackOnCorruptCache verifies a corrupt on-disk
// cache doesn't strand startup — it falls through to the embedded copy.
func TestLoadBootstrapConfig_FallsBackOnCorruptCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), configCacheName)
	require.NoError(t, os.WriteFile(cache, []byte("not a gzip config"), 0o600))

	cfg, err := loadBootstrapConfig(cache)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, cfg.Providers)
}

// TestLoadBootstrapConfig_PrefersOnDiskCache verifies the on-disk cache is read
// in preference to the embedded copy. The embedded fallback is corrupted for
// the duration of the test so a successful parse can only have come from disk.
func TestLoadBootstrapConfig_PrefersOnDiskCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), configCacheName)
	// Seed the cache with the (valid) embedded bytes before corrupting the
	// in-memory fallback, so disk holds a good config and embedded does not.
	require.NoError(t, os.WriteFile(cache, embeddedConfig, 0o600))

	orig := embeddedConfig
	t.Cleanup(func() { embeddedConfig = orig })
	embeddedConfig = []byte("corrupt")

	cfg, err := loadBootstrapConfig(cache)
	require.NoError(t, err, "must succeed from the on-disk cache with the embedded fallback broken")
	require.NotNil(t, cfg)
	assert.NotEmpty(t, cfg.Providers)
}

// TestFetchAndCacheConfig_PersistsValidConfig verifies a good response is
// validated and written to the cache path for the next cold start.
func TestFetchAndCacheConfig_PersistsValidConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(embeddedConfig)
	}))
	defer srv.Close()

	cache := filepath.Join(t.TempDir(), configCacheName)
	err := fetchAndCacheConfig(context.Background(), srv.Client(), srv.URL, cache)
	require.NoError(t, err)

	written, err := os.ReadFile(cache)
	require.NoError(t, err, "config should be persisted to the cache path")
	assert.Equal(t, embeddedConfig, written)
}

// TestFetchAndCacheConfig_NoWriteOnError verifies a failed fetch neither errors
// silently nor clobbers an existing cache — a previously good config survives.
func TestFetchAndCacheConfig_NoWriteOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache := filepath.Join(t.TempDir(), configCacheName)
	seed := []byte("previous-good-config")
	require.NoError(t, os.WriteFile(cache, seed, 0o600))

	err := fetchAndCacheConfig(context.Background(), srv.Client(), srv.URL, cache)
	require.Error(t, err)

	got, readErr := os.ReadFile(cache)
	require.NoError(t, readErr)
	assert.Equal(t, seed, got, "a failed fetch must leave the existing cache untouched")
}
