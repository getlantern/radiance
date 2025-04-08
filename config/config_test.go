package config

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"
	"github.com/getlantern/eventual/v2"
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
		config:       eventual.NewValue(),
	}

	// Test case: No config set
	t.Run("NoConfigSet", func(t *testing.T) {
		_, err := ch.GetConfig()
		require.Error(t, err, "Expected error when no config is set")
		assert.Contains(t, err.Error(), "context canceled", "Error message should indicate nil config")
	})

	// Test case: Valid config set
	t.Run("ValidConfigSet", func(t *testing.T) {
		expectedConfig := &C.ConfigResponse{
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
				{Country: "UK", City: "London"},
			},
		}

		// Set the config
		ch.config.Set(expectedConfig)

		// Retrieve the config
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when config is set")
		assert.Equal(t, expectedConfig, actualConfig, "Retrieved config should match the expected config")
	})
}
func TestSetConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, configFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath:   configPath,
		configParser: mockConfigParser,
		config:       eventual.NewValue(),
	}

	// Mock listener to verify notifications
	var notifiedOldConfig, notifiedNewConfig *Config
	var notified atomic.Bool
	mockListener := func(oldConfig, newConfig *Config) error {
		notifiedOldConfig = oldConfig
		notifiedNewConfig = newConfig
		notified.Store(true)
		return nil
	}
	ch.AddConfigListener(mockListener)

	// Test case: Setting a new config
	t.Run("SetNewConfig", func(t *testing.T) {
		newConfig := &Config{
			ConfigResponse: C.ConfigResponse{
				Servers: []C.ServerLocation{
					{Country: "US", City: "New York"},
				},
			},
		}

		ch.setConfig(newConfig)

		// Verify the config is set
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when config is set")
		assert.Equal(t, newConfig, actualConfig, "Config should match the newly set config")

		// Verify the listener was notified
		tries := 0
		for !notified.Load() && tries < 1000 {
			// Wait for the notification
			time.Sleep(10 * time.Millisecond)
			tries++
		}
		assert.Nil(t, notifiedOldConfig, "Old config should be nil for the first set")
		assert.Equal(t, newConfig, notifiedNewConfig, "New config should match the newly set config")
	})

	// Test case: Updating an existing config
	t.Run("UpdateExistingConfig", func(t *testing.T) {
		notified.Store(false) // Reset notification flag
		notifiedNewConfig = nil
		notifiedOldConfig = nil
		existingConfig := &Config{
			ConfigResponse: C.ConfigResponse{
				Servers: []C.ServerLocation{
					{Country: "US", City: "New York"},
				},
			},
		}
		ch.setConfig(existingConfig)
		tries := 0
		for !notified.Load() && tries < 1000 {
			// Wait for the notification
			time.Sleep(10 * time.Millisecond)
			tries++
		}

		notified.Store(false) // Reset notification flag
		notifiedNewConfig = nil
		notifiedOldConfig = nil
		updatedConfig := &Config{
			ConfigResponse: C.ConfigResponse{
				Servers: []C.ServerLocation{
					{Country: "UK", City: "London"},
				},
			},
		}
		ch.setConfig(updatedConfig)

		// Verify the config is updated
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when config is updated")
		expectedConfig := &Config{
			ConfigResponse: C.ConfigResponse{
				Servers: []C.ServerLocation{
					{Country: "UK", City: "London"},
				},
			},
		}
		assert.Equal(t, expectedConfig, actualConfig, "Config should be merged with the updated config")

		// Verify the listener was notified
		tries = 0
		for !notified.Load() && tries < 1000 {
			// Wait for the notification
			time.Sleep(10 * time.Millisecond)
			tries++
		}
		assert.True(t, notified.Load(), "Listener should be notified after setting config")
		assert.Equal(t, existingConfig, notifiedOldConfig, "Old config should match the previous config")
		assert.Equal(t, expectedConfig, notifiedNewConfig, "New config should match the merged config")
	})

	// Test case: Handling nil config
	t.Run("NilConfig", func(t *testing.T) {
		newConfig := &Config{
			ConfigResponse: C.ConfigResponse{
				Servers: []C.ServerLocation{
					{Country: "US", City: "New York"},
				},
			},
		}
		ch.setConfig(newConfig)
		ch.setConfig(nil)

		// Verify the config remains unchanged
		actualConfig, err := ch.GetConfig()
		require.NoError(t, err, "Should not return an error when setting nil config")
		assert.NotNil(t, actualConfig, "Config should not be nil after setting nil config")
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
		config:       eventual.NewValue(),
		ftr:          newFetcher(http.DefaultClient, &UserStub{}),
	}

	ch.config.Set(&Config{
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
