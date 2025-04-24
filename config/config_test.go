package config

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"
	"github.com/getlantern/radiance/user"
)

// Mock implementation of ConfigParser for testing
func mockConfigParser(data []byte) (*Config, error) {
	var cfg Config
	err := json.Unmarshal(data, &cfg)
	return &cfg, err
}

func TestSaveConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, configFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath:   configPath,
		configParser: mockConfigParser,
	}

	// Create a sample config to save
	expectedConfig := &Config{
		ConfigResponse: C.ConfigResponse{
			// Populate with sample data
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
				{Country: "UK", City: "London"},
			},
		},
	}
	// Save the config
	ch.saveConfig(expectedConfig)

	// Verify the file exists
	_, err := os.Stat(configPath)
	require.NoError(t, err, "Config file should exist")

	// Read the file content
	data, err := os.ReadFile(configPath)
	require.NoError(t, err, "Should be able to read the config file")

	// Parse the content using the mock parser
	actualConfig, err := ch.configParser(data)
	require.NoError(t, err, "Should be able to parse the config file")

	// Verify the content matches the expected config
	assert.Equal(t, expectedConfig, actualConfig, "Saved config should match the expected config")
}
func TestGetConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, configFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath:   configPath,
		configParser: mockConfigParser,
		config:       atomic.Value{},
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
			ConfigResponse: C.ConfigResponse{
				Servers: []C.ServerLocation{
					{Country: "US", City: "New York"},
					{Country: "UK", City: "London"},
				},
			},
		}

		ch.config.Store(expectedConfig)

		// Retrieve the config
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when config is set")
		assert.Equal(t, expectedConfig, actualConfig, "Retrieved config should match the expected config")
	})
}

func TestSetPreferredServerLocation(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, configFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath:   configPath,
		configParser: mockConfigParser,
		config:       atomic.Value{},
		ftr:          newFetcher(http.DefaultClient, &UserStub{}),
	}

	ch.config.Store(&Config{
		ConfigResponse: C.ConfigResponse{
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
				{Country: "UK", City: "London"},
			},
		},
		PreferredLocation: C.ServerLocation{
			Country: "US",
			City:    "New York",
		},
	})

	// Test case: Set preferred server location
	t.Run("SetPreferredServerLocation", func(t *testing.T) {
		country := "US"
		city := "Los Angeles"

		// Call SetPreferredServerLocation
		ch.SetPreferredServerLocation(country, city)

		// Verify the preferred location is updated
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when getting config")
		assert.Equal(t, country, actualConfig.PreferredLocation.Country, "Preferred country should match")
		assert.Equal(t, city, actualConfig.PreferredLocation.City, "Preferred city should match")
	})
}

func TestHandlerFetchConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, configFileName)

	// Mock fetcher
	mockFetcher := &MockFetcher{}

	// Create a ConfigHandler with the mock parser and fetcher
	ch := &ConfigHandler{
		configPath:        configPath,
		configParser:      mockConfigParser,
		config:            atomic.Value{},
		preferredLocation: atomic.Value{},
		ftr:               mockFetcher,
	}

	// Test case: No server location set
	t.Run("NoServerLocationSet", func(t *testing.T) {
		mockFetcher.response = []byte(`{
			"ConfigResponse": {
				"Servers": [
					{"Country": "US", "City": "New York"},
					{"Country": "UK", "City": "London"}
				]
			}
		}`)

		err := ch.fetchConfig()
		require.NoError(t, err, "Should not return an error when no server location is set")
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when getting config")
		assert.Equal(t, "US", actualConfig.ConfigResponse.Servers[0].Country, "First server country should match")
		assert.Equal(t, "New York", actualConfig.ConfigResponse.Servers[0].City, "First server city should match")
	})

	// Test case: No stored config, fetch succeeds
	t.Run("NoStoredConfigFetchSuccess", func(t *testing.T) {
		mockFetcher.response = []byte(`{
			"ConfigResponse": {
				"Servers": [
					{"Country": "US", "City": "New York"},
					{"Country": "UK", "City": "London"}
				]
			}
		}`)
		mockFetcher.err = nil

		ch.preferredLocation.Store(C.ServerLocation{Country: "US", City: "New York"})

		err := ch.fetchConfig()
		require.NoError(t, err, "Should not return an error when fetch succeeds")

		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when getting config")
		assert.Equal(t, "US", actualConfig.ConfigResponse.Servers[0].Country, "First server country should match")
		assert.Equal(t, "New York", actualConfig.ConfigResponse.Servers[0].City, "First server city should match")
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

// MockFetcher is a mock implementation of the fetcher used for testing
type MockFetcher struct {
	response []byte
	err      error
}

func (mf *MockFetcher) fetchConfig(preferred C.ServerLocation) ([]byte, error) {
	return mf.response, mf.err
}

type UserStub struct{}

// Verify that a UserStub implements the User interface
var _ user.BaseUser = (*UserStub)(nil)

func (u *UserStub) DeviceID() string {
	return "test-device-id"
}
func (u *UserStub) LegacyID() int64 {
	return 123456789
}
func (u *UserStub) LegacyToken() string {
	return "test-legacy-token"
}
