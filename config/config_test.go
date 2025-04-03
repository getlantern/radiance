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
				ctx, cancel = context.WithTimeout(ctx, 1*time.Millisecond)
				defer cancel()
				time.Sleep(2 * time.Millisecond) // Ensure context expires
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
