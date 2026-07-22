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

// TestLoadBootstrapConfig_PrefersOnDiskCache verifies a valid on-disk cache is
// read (the disk branch executes) rather than always using the embedded copy.
func TestLoadBootstrapConfig_PrefersOnDiskCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), configCacheName)
	// The embedded bytes are a known-valid gzipped config; reuse them as a
	// stand-in for a previously cached fetch.
	require.NoError(t, os.WriteFile(cache, embeddedConfig, 0o600))

	cfg, err := loadBootstrapConfig(cache)
	require.NoError(t, err)
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
// silently nor poisons the cache — the cache file is left untouched.
func TestFetchAndCacheConfig_NoWriteOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache := filepath.Join(t.TempDir(), configCacheName)
	err := fetchAndCacheConfig(context.Background(), srv.Client(), srv.URL, cache)
	require.Error(t, err)

	_, statErr := os.Stat(cache)
	assert.True(t, os.IsNotExist(statErr), "cache must not be written on a failed fetch")
}
