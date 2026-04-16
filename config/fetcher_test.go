package config

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	C "github.com/getlantern/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
)

func TestFetchConfig(t *testing.T) {
	settings.InitSettings(t.TempDir())
	settings.Set(settings.DeviceIDKey, "mock-device-id")
	settings.Set(settings.UserIDKey, 1234567890)
	settings.Set(settings.TokenKey, "mock-legacy-token")

	privateKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	tests := []struct {
		name               string
		preferredServerLoc *C.ServerLocation
		serverStatus       int
		serverBody         string
		expectedConfig     []byte
		expectError        bool
	}{
		{
			name: "successful fetch",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			serverStatus:   http.StatusOK,
			serverBody:     `{"key":"value"}`,
			expectedConfig: []byte(`{"key":"value"}`),
		},
		{
			name: "no new config available",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			serverStatus:   http.StatusNotModified,
			expectedConfig: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedReq *http.Request
			var capturedBody []byte

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				capturedBody = body
				capturedReq = r
				w.WriteHeader(tt.serverStatus)
				if tt.serverBody != "" {
					w.Write([]byte(tt.serverBody))
				}
			}))
			defer srv.Close()

			f := newFetcher("en-US", nil, srv.Client()).(*fetcher)
			f.baseURL = srv.URL

			gotConfig, err := f.fetchConfig(t.Context(), *tt.preferredServerLoc, privateKey.PublicKey().String())

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedConfig, gotConfig)
			}

			require.NotNil(t, capturedReq)
			assert.Equal(t, "application/json", capturedReq.Header.Get("Content-Type"))
			assert.Equal(t, "no-cache", capturedReq.Header.Get("Cache-Control"))

			var confReq C.ConfigRequest
			err = json.Unmarshal(capturedBody, &confReq)
			require.NoError(t, err)

			assert.Equal(t, common.Platform, confReq.Platform)
			assert.Equal(t, common.Name, confReq.AppName)
			assert.Equal(t, settings.GetString(settings.DeviceIDKey), confReq.DeviceID)
			assert.Equal(t, privateKey.PublicKey().String(), confReq.WGPublicKey)
			if tt.preferredServerLoc != nil {
				assert.Equal(t, tt.preferredServerLoc, confReq.PreferredLocation)
			}
		})
	}
}
