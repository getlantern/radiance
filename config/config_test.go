package config

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	C "github.com/getlantern/common"
	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/radiance/user"
	"github.com/stretchr/testify/assert"

	O "github.com/sagernet/sing-box/option"
)

func TestConfigHandler_GetConfig(t *testing.T) {
	tests := []struct {
		name         string
		configResult interface{}
		expectedErr  error
		expectedCfg  *C.ConfigResponse
	}{
		{
			name: "returns current config",
			configResult: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
			expectedErr: nil,
			expectedCfg: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
		},
		{
			name:         "context expired",
			configResult: nil,
			expectedErr:  context.DeadlineExceeded,
			expectedCfg:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &ConfigHandler{config: eventual.NewValue()}
			if tt.configResult != nil {
				ch.config.Set(tt.configResult)
			}

			ctx := context.Background()
			if tt.expectedErr == context.DeadlineExceeded {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-1*time.Second))
				defer cancel()
			}

			got, err := ch.GetConfig(ctx)
			if tt.expectedErr != nil {
				assert.ErrorIs(t, err, tt.expectedErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expectedCfg, got)
		})
	}
}
func TestNewConfigHandler(t *testing.T) {
	tests := []struct {
		name          string
		pollInterval  time.Duration
		httpClient    *http.Client
		user          *user.User
		dataDir       string
		expectedError bool
	}{
		{
			name:         "valid inputs",
			pollInterval: 1 * time.Second,
			httpClient:   &http.Client{},
			user:         &user.User{},
			dataDir:      t.TempDir(),
		},
		{
			name:          "invalid data directory",
			pollInterval:  1 * time.Second,
			httpClient:    &http.Client{},
			user:          &user.User{},
			dataDir:       "/invalid/path",
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := NewConfigHandler(tt.pollInterval, tt.httpClient, tt.user, tt.dataDir)

			assert.NotNil(t, ch, "ConfigHandler should not be nil")
			assert.NotNil(t, ch.config, "ConfigHandler.config should not be nil")
			assert.NotNil(t, ch.stopC, "ConfigHandler.stopC should not be nil")
			assert.NotNil(t, ch.closeOnce, "ConfigHandler.closeOnce should not be nil")
			assert.Equal(t, filepath.Join(tt.dataDir, configFileName), ch.configPath, "ConfigHandler.configPath should be set correctly")
			assert.NotNil(t, ch.apiClient, "ConfigHandler.apiClient should not be nil")
			assert.NotNil(t, ch.ftr, "ConfigHandler.ftr should not be nil")
			assert.NotNil(t, ch.preferredServerLocation.Load(), "ConfigHandler.preferredServerLocation should not be nil")
			assert.Len(t, ch.configListeners, 1, "ConfigHandler.configListeners should have one listener")

			// Check if the config was loaded correctly
			cfg, _ := ch.config.Get(eventual.DontWait)
			if tt.expectedError {
				assert.Nil(t, cfg, "ConfigHandler.config should be nil")
			} else {
				assert.NoError(t, ch.loadConfig(), "ConfigHandler.loadConfig should not return an error")
			}

			// Stop the fetch loop to clean up
			ch.Stop()
		})
	}
}
func TestConfigHandler_mergeConfig(t *testing.T) {
	tests := []struct {
		name           string
		existingConfig *C.ConfigResponse
		newConfig      *C.ConfigResponse
		expectedConfig *C.ConfigResponse
		expectMergeErr bool
	}{
		{
			name: "override existing outbound config",
			existingConfig: &C.ConfigResponse{
				Options: O.Options{
					Outbounds: []O.Outbound{
						{
							Tag:  "outbound",
							Type: "direct",
						},
					},
				},
			},
			newConfig: &C.ConfigResponse{
				Options: O.Options{
					Outbounds: []O.Outbound{
						{
							Tag:  "outbound",
							Type: "proxied",
						},
					},
				},
			},
			expectedConfig: &C.ConfigResponse{
				Options: O.Options{
					Outbounds: []O.Outbound{
						{
							Tag:  "outbound",
							Type: "proxied",
						},
					},
				},
			},
			expectMergeErr: false,
		},
		{
			name: "kill previous outbounds but keep other stuff",
			existingConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
				Options: O.Options{
					Outbounds: []O.Outbound{
						{
							Tag:  "outbound",
							Type: "direct",
						},
						{
							Tag:  "outbound2",
							Type: "direct",
						},
						{
							Tag:  "outbound3",
							Type: "direct",
						},
					},
				},
			},
			newConfig: &C.ConfigResponse{
				Options: O.Options{
					Outbounds: []O.Outbound{
						{
							Tag:  "outbound",
							Type: "proxied",
						},
					},
				},
			},
			expectedConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
				Options: O.Options{
					Outbounds: []O.Outbound{
						{
							Tag:  "outbound",
							Type: "proxied",
						},
					},
				},
			},
			expectMergeErr: false,
		},
		{
			name: "merge new config into existing config",
			existingConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
			newConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{IP: "192.168.1.1"},
			},
			expectedConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US", IP: "192.168.1.1"},
			},
			expectMergeErr: false,
		},
		{
			name: "override existing value",
			existingConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
			newConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "UK", IP: "192.168.1.1"},
			},
			expectedConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "UK", IP: "192.168.1.1"},
			},
			expectMergeErr: false,
		},
		{
			name: "keep existing values",
			existingConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US", IP: "192.168.1.1"},
			},
			newConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "UK"},
			},
			expectedConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "UK", IP: "192.168.1.1"},
			},
			expectMergeErr: false,
		},
		{
			name:           "set new config when no existing config",
			existingConfig: nil,
			newConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
			expectedConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
			expectMergeErr: false,
		},
		{
			name: "handle merge error gracefully",
			existingConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
			newConfig: nil, // Invalid new config to simulate merge error
			expectedConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{Country: "US"},
			},
			expectMergeErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &ConfigHandler{
				config: eventual.NewValue(),
			}
			if tt.existingConfig != nil {
				ch.config.Set(tt.existingConfig)
			}

			err := ch.mergeConfig(tt.newConfig)
			if tt.expectMergeErr {
				assert.Error(t, err, "Expected an error but got none")
			} else {
				assert.NoError(t, err, "Expected no error but got one")
			}

			// Verify the resulting config
			resultConfig, _ := ch.config.Get(eventual.DontWait)
			assert.Equal(t, tt.expectedConfig, resultConfig)
		})
	}
}
