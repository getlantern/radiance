package config

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"
	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
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

type UserStub struct{}

// Verify that a UserStub implements the User interface
var _ common.UserInfo = (*UserStub)(nil)

func (u *UserStub) Locale() string {
	return "en-US"
}
func (u *UserStub) DeviceID() string {
	return "test-device-id"
}
func (u *UserStub) LegacyID() int64 {
	return 123456789
}
func (u *UserStub) LegacyToken() string {
	return "test-legacy-token"
}
func (u *UserStub) Save(data *protos.LoginResponse) error {
	return nil
}
func (u *UserStub) GetUserData() (*protos.LoginResponse, error) {
	return &protos.LoginResponse{
		LegacyID:    123456789,
		LegacyToken: "test-legacy-token",
	}, nil
}
func (u *UserStub) ReadSalt() ([]byte, error) {
	return []byte("test-salt"), nil
}
func (u *UserStub) WriteSalt(salt []byte) error {
	return nil
}
