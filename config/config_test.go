package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/getlantern/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
)

func TestSaveConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, internal.ConfigFileName)

	// Create a sample config to save
	expectedConfig := Config{
		// Populate with sample data
		Servers: []C.ServerLocation{
			{Country: "US", City: "New York"},
			{Country: "UK", City: "London"},
		},
	}
	// Save the config
	err := saveConfig(&expectedConfig, configPath)
	require.NoError(t, err, "Should not return an error when saving config")

	// Read the file content
	data, err := os.ReadFile(configPath)
	require.NoError(t, err, "Should be able to read the config file")

	var actualConfig Config
	err = json.Unmarshal(data, &actualConfig)
	require.NoError(t, err, "Should be able to parse the config file")

	// Verify the content matches the expected config
	assert.Equal(t, expectedConfig, actualConfig, "Saved config should match the expected config")
}
func TestGetConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, internal.ConfigFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath: configPath,
	}

	// Test case: No config set
	t.Run("NoConfigSet", func(t *testing.T) {
		_, err := ch.GetConfig()
		require.Error(t, err, "Expected error when no config is set")
		assert.Contains(t, err.Error(), "no config", "Error message should indicate nil config")
	})

	// Test case: Valid config set
	t.Run("ValidConfigSet", func(t *testing.T) {
		expectedConfig := &Config{
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
				{Country: "UK", City: "London"},
			},
		}

		ch.config.Store(expectedConfig)

		// Retrieve the config
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when config is set")
		assert.Equal(t, expectedConfig, actualConfig, "Retrieved config should match the expected config")
	})
}

func TestHandlerFetchConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, internal.ConfigFileName)

	// Mock fetcher
	mockFetcher := &MockFetcher{}

	// Create a ConfigHandler with the mock parser and fetcher
	ctx, cancel := context.WithCancel(context.Background())
	ch := &ConfigHandler{
		configPath: configPath,
		ftr:        mockFetcher,
		wgKeyPath:  filepath.Join(tempDir, "wg.key"),
		ctx:        ctx,
		cancel:     cancel,
		logger:     log.NoOpLogger(),
	}

	// Test case: No server location set
	t.Run("NoServerLocationSet", func(t *testing.T) {
		mockFetcher.response = []byte(`{
				"Servers": [
					{"Country": "US", "City": "New York"},
					{"Country": "UK", "City": "London"}
				]
		}`)

		err := ch.fetchConfig()
		require.NoError(t, err, "Should not return an error when no server location is set")
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when getting config")
		assert.Equal(t, "US", actualConfig.Servers[0].Country, "First server country should match")
		assert.Equal(t, "New York", actualConfig.Servers[0].City, "First server city should match")
	})

	// Test case: No stored config, fetch succeeds
	t.Run("NoStoredConfigFetchSuccess", func(t *testing.T) {
		mockFetcher.response = []byte(`{
				"Servers": [
					{"Country": "US", "City": "New York"},
					{"Country": "UK", "City": "London"}
				]
		}`)
		mockFetcher.err = nil

		err := ch.fetchConfig()
		require.NoError(t, err, "Should not return an error when fetch succeeds")

		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when getting config")
		assert.Equal(t, "US", actualConfig.Servers[0].Country, "First server country should match")
		assert.Equal(t, "New York", actualConfig.Servers[0].City, "First server city should match")
	})

	// Test case: Fetch fails
	t.Run("FetchFails", func(t *testing.T) {
		mockFetcher.response = nil
		mockFetcher.err = errors.New("fetch error")

		err := ch.fetchConfig()
		require.Error(t, err, "Should return an error when fetch fails")
		assert.Contains(t, err.Error(), "fetch error", "Error message should contain fetch error")
	})

	// Test case: Fetch returns nil response
	t.Run("FetchReturnsNilResponse", func(t *testing.T) {
		mockFetcher.response = nil
		mockFetcher.err = nil

		err := ch.fetchConfig()
		require.NoError(t, err, "Should not return an error when fetch returns nil response")
	})

	// Test case: Config parsing fails
	t.Run("ConfigParsingFails", func(t *testing.T) {
		mockFetcher.response = []byte(`invalid json`)
		mockFetcher.err = nil

		err := ch.fetchConfig()
		require.Error(t, err, "Should return an error when config parsing fails")
		assert.Contains(t, err.Error(), "parsing config", "Error message should indicate parsing error")
	})
}

// TestFetchConfigCoalescesConcurrentRequest verifies that a fetchConfig call
// arriving while another fetch is in flight returns immediately and causes
// the in-flight fetch to re-run exactly once after it completes.
func TestFetchConfigCoalescesConcurrentRequest(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, internal.ConfigFileName)

	release := make(chan struct{})
	bf := &BlockingFetcher{
		response: []byte(`{"Servers":[{"Country":"US","City":"New York"}]}`),
		entered:  make(chan struct{}, 2),
		release:  release,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := &ConfigHandler{
		configPath: configPath,
		ftr:        bf,
		wgKeyPath:  filepath.Join(tempDir, "wg.key"),
		ctx:        ctx,
		cancel:     cancel,
		logger:     log.NoOpLogger(),
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- ch.fetchConfig() }()

	select {
	case <-bf.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first fetch never started")
	}

	require.NoError(t, ch.fetchConfig(), "concurrent call should return nil while a fetch is in flight")

	close(release)
	require.NoError(t, <-firstDone, "first fetch should complete cleanly")

	select {
	case <-bf.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced follow-up fetch never started")
	}

	assert.Equal(t, int32(2), bf.calls.Load(), "expected exactly two fetches: original + coalesced follow-up")
}

// Make sure MockFetcher implements the Fetcher interface
var _ Fetcher = (*MockFetcher)(nil)

// MockFetcher is a mock implementation of the fetcher used for testing
type MockFetcher struct {
	response []byte
	err      error
}

func (mf *MockFetcher) fetchConfig(ctx context.Context, preferred C.ServerLocation, wgPublicKey string) ([]byte, error) {
	return mf.response, mf.err
}

var _ Fetcher = (*BlockingFetcher)(nil)

// BlockingFetcher is a test Fetcher that signals entry into each fetchConfig
// call and blocks until release is closed.
type BlockingFetcher struct {
	response []byte
	err      error
	entered  chan struct{}
	release  <-chan struct{}
	calls    atomic.Int32
}

func (bf *BlockingFetcher) fetchConfig(ctx context.Context, preferred C.ServerLocation, wgPublicKey string) ([]byte, error) {
	bf.calls.Add(1)
	select {
	case bf.entered <- struct{}{}:
	default:
	}
	<-bf.release
	return bf.response, bf.err
}

