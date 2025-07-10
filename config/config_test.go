package config

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	C "github.com/getlantern/common"
	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
)

func TestSaveConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, common.ConfigFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath: configPath,
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
	err := saveConfig(expectedConfig, configPath)
	require.NoError(t, err, "Should not return an error when saving config")

	// Read the file content
	data, err := os.ReadFile(configPath)
	require.NoError(t, err, "Should be able to read the config file")

	// Parse the content using the mock parser
	actualConfig, err := ch.unmarshalConfig(data)
	require.NoError(t, err, "Should be able to parse the config file")

	// Verify the content matches the expected config
	assert.Equal(t, expectedConfig, actualConfig, "Saved config should match the expected config")
}
func TestGetConfig(t *testing.T) {
	// Setup temporary directory for testing
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, common.ConfigFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath: configPath,
		config:     atomic.Value{},
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
	configPath := filepath.Join(tempDir, common.ConfigFileName)

	// Create a ConfigHandler with the mock parser
	ch := &ConfigHandler{
		configPath: configPath,
		config:     atomic.Value{},
		ftr:        newFetcher(http.DefaultClient, &UserStub{}, "en-US"),
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
	configPath := filepath.Join(tempDir, common.ConfigFileName)

	// Mock fetcher
	mockFetcher := &MockFetcher{}

	// Create a ConfigHandler with the mock parser and fetcher
	ch := &ConfigHandler{
		configPath:        configPath,
		config:            atomic.Value{},
		preferredLocation: atomic.Value{},
		ftr:               mockFetcher,
		wgKeyPath:         filepath.Join(tempDir, "wg.key"),
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
		assert.Equal(t, "US", actualConfig.ConfigResponse.Servers[0].Country, "First server country should match")
		assert.Equal(t, "New York", actualConfig.ConfigResponse.Servers[0].City, "First server city should match")
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

func TestMergeResp(t *testing.T) {
	t.Run("SuccessfulMerge", func(t *testing.T) {
		oldConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP:       "1.1.1.1",
				ProToken: "test-pro-token",
			},
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
			},
			Options: O.Options{},
		}
		newConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP: "2.2.2.2",
			},
			Servers: []C.ServerLocation{
				{Country: "UK", City: "London"},
			},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound1",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}

		mergedConfig, err := mergeResp(oldConfig, newConfig)
		require.NoError(t, err, "Should not return an error when merging configs")
		assert.Equal(t, newConfig.Servers, mergedConfig.Servers, "Merged servers should match newConfig servers")
		assert.Equal(t, newConfig.Options, mergedConfig.Options, "Merged options should match newConfig options")
		assert.Equal(t, newConfig.UserInfo.IP, mergedConfig.UserInfo.IP, "Merged IP should match newConfig IP")
		assert.Equal(t, oldConfig.UserInfo.ProToken, mergedConfig.UserInfo.ProToken, "Merged ProToken should match oldConfig ProToken")
	})

	t.Run("NoNewServerLocations", func(t *testing.T) {
		oldConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP:       "1.1.1.1",
				ProToken: "test-pro-token",
			},
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
			},
			Options: O.Options{},
		}
		newConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP: "2.2.2.2",
			},
			Servers: []C.ServerLocation{},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound1",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}

		mergedConfig, err := mergeResp(oldConfig, newConfig)
		require.NoError(t, err, "Should not return an error when merging configs")
		assert.Equal(t, oldConfig.Servers, mergedConfig.Servers, "Merged servers should match oldConfig servers")
		assert.Equal(t, newConfig.Options, mergedConfig.Options, "Merged options should match newConfig options")
		assert.Equal(t, newConfig.UserInfo.IP, mergedConfig.UserInfo.IP, "Merged IP should match newConfig IP")
		assert.Equal(t, oldConfig.UserInfo.ProToken, mergedConfig.UserInfo.ProToken, "Merged ProToken should match oldConfig ProToken")
	})
	t.Run("OverwriteOutbounds", func(t *testing.T) {
		oldConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP:       "1.1.1.1",
				ProToken: "test-pro-token",
			},
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
			},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound3",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}
		newConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP: "2.2.2.2",
			},
			Servers: []C.ServerLocation{},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound1",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}

		mergedConfig, err := mergeResp(oldConfig, newConfig)
		require.NoError(t, err, "Should not return an error when merging configs")
		assert.Equal(t, oldConfig.Servers, mergedConfig.Servers, "Merged servers should match oldConfig servers")
		assert.Equal(t, newConfig.Options.Outbounds, mergedConfig.Options.Outbounds, "Merged options should match newConfig options")
		assert.Equal(t, newConfig.UserInfo.IP, mergedConfig.UserInfo.IP, "Merged IP should match newConfig IP")
		assert.Equal(t, oldConfig.UserInfo.ProToken, mergedConfig.UserInfo.ProToken, "Merged ProToken should match oldConfig ProToken")
	})
	t.Run("KeepDNSOptions", func(t *testing.T) {
		oldConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP:       "1.1.1.1",
				ProToken: "test-pro-token",
			},
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
			},
			Options: O.Options{
				DNS: &O.DNSOptions{
					ReverseMapping: true,
					Servers: []O.DNSServerOptions{
						{
							Tag:     "dns1",
							Address: "8.8.8.8",
						},
					},
				},
			},
		}
		newConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP: "2.2.2.2",
			},
			Servers: []C.ServerLocation{},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound1",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}

		mergedConfig, err := mergeResp(oldConfig, newConfig)
		require.NoError(t, err, "Should not return an error when merging configs")
		assert.Equal(t, oldConfig.Servers, mergedConfig.Servers, "Merged servers should match oldConfig servers")
		assert.Equal(t, newConfig.Options.Outbounds, mergedConfig.Options.Outbounds, "Merged Outbounds should match newConfig Outbounds")
		assert.Equal(t, newConfig.UserInfo.IP, mergedConfig.UserInfo.IP, "Merged IP should match newConfig IP")
		assert.Equal(t, oldConfig.UserInfo.ProToken, mergedConfig.UserInfo.ProToken, "Merged ProToken should match oldConfig ProToken")
		assert.Equal(t, oldConfig.Options.DNS, mergedConfig.Options.DNS, "Merged DNS options should match oldConfig DNS options")
	})
	t.Run("SuccessfulRemovedUnassignedOutbounds", func(t *testing.T) {
		oldConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP:       "1.1.1.1",
				ProToken: "test-pro-token",
			},
			Servers: []C.ServerLocation{
				{Country: "US", City: "New York"},
			},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound1",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
					{
						Tag: "outbound4",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}
		newConfig := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP: "2.2.2.2",
			},
			Servers: []C.ServerLocation{
				{Country: "UK", City: "London"},
			},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound2",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}
		want := &C.ConfigResponse{
			UserInfo: C.UserInfo{
				IP:       "2.2.2.2",
				ProToken: "test-pro-token",
			},
			Servers: []C.ServerLocation{
				{Country: "UK", City: "London"},
			},
			Options: O.Options{
				Outbounds: []O.Outbound{
					{
						Tag: "outbound2",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}
		mergedConfig, err := mergeResp(oldConfig, newConfig)
		require.NoError(t, err, "Should not return an error when merging configs")
		assert.Equal(t, mergedConfig, want)
	})

	t.Run("DoNotOverwriteAllOptions", func(t *testing.T) {
		oldConfig := &C.ConfigResponse{
			Options: O.Options{
				DNS: &O.DNSOptions{
					ReverseMapping: true,
					Servers: []O.DNSServerOptions{
						{
							Tag:     "dns1",
							Address: "8.8.8.8",
						},
					},
				},
				Outbounds: []O.Outbound{
					{
						Tag: "outbound1",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
					{
						Tag: "outbound4",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}
		newConfig := &C.ConfigResponse{
			Options: O.Options{
				DNS: &O.DNSOptions{
					Servers: []O.DNSServerOptions{
						{
							Tag:     "dns1",
							Address: "1.1.1.1",
						},
					},
				},
				Outbounds: []O.Outbound{
					{
						Tag: "outbound2",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}
		want := &C.ConfigResponse{
			Options: O.Options{
				DNS: &O.DNSOptions{
					ReverseMapping: true,
					Servers: []O.DNSServerOptions{
						{
							Tag:     "dns1",
							Address: "1.1.1.1",
						},
					},
				},
				Outbounds: []O.Outbound{
					{
						Tag: "outbound2",
						Options: map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		}
		mergedConfig, err := mergeResp(oldConfig, newConfig)
		require.NoError(t, err, "Should not return an error when merging configs")
		assert.Equal(t, mergedConfig, want)
	})
}

// MockFetcher is a mock implementation of the fetcher used for testing
type MockFetcher struct {
	response []byte
	err      error
}

func (mf *MockFetcher) fetchConfig(preferred C.ServerLocation, wgPublicKey string) ([]byte, error) {
	return mf.response, mf.err
}

type UserStub struct{}

// Verify that a UserStub implements the User interface
var _ common.UserInfo = (*UserStub)(nil)

func (u *UserStub) GetData() (*protos.LoginResponse, error) {
	return &protos.LoginResponse{
		LegacyID:    123456789,
		LegacyToken: "test-legacy-token",
	}, nil
}
func (u *UserStub) Locale() string                           { return "en-US" }
func (u *UserStub) DeviceID() string                         { return "test-device-id" }
func (u *UserStub) LegacyID() int64                          { return 123456789 }
func (u *UserStub) LegacyToken() string                      { return "test-legacy-token" }
func (u *UserStub) SetData(data *protos.LoginResponse) error { return nil }
func (u *UserStub) SetLocale(locale string)                  {}
