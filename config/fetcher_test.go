package config

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	C "github.com/getlantern/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/common"
)

type mockRoundTripper struct {
	req  *http.Request
	resp *http.Response
	err  error
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.req = req
	return m.resp, m.err
}

type mockUser struct {
	common.UserInfo
}

func (m *mockUser) DeviceID() string {
	return "mock-device-id"
}
func (m *mockUser) LegacyID() int64 {
	return 1234567890
}
func (m *mockUser) AuthToken() string {
	return "mock-auth-token"
}
func (m *mockUser) LegacyToken() string {
	return "mock-legacy-token"
}

func TestFetchConfig(t *testing.T) {
	mockUser := &mockUser{}
	privateKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	tests := []struct {
		name                 string
		preferredServerLoc   *C.ServerLocation
		mockResponse         *http.Response
		mockError            error
		expectedConfig       []byte
		expectedErrorMessage string
	}{
		{
			name: "successful fetch with new config",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader(func() []byte {
					data := []byte(`{"key":"value"}`)
					return data
				}())),
			},
			expectedConfig: []byte(`{"key":"value"}`),
		},
		{
			name: "no new config available",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusNotModified,
				Body:       io.NopCloser(bytes.NewReader(nil)),
			},
			expectedConfig: nil,
		},
		{
			name: "error during request",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockError:            context.DeadlineExceeded,
			expectedErrorMessage: "context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRT := &mockRoundTripper{
				resp: tt.mockResponse,
				err:  tt.mockError,
			}
			fetcher := newFetcher(&http.Client{
				Transport: mockRT,
			}, mockUser, "en-US", &api.APIClient{})

			gotConfig, err := fetcher.fetchConfig(t.Context(), *tt.preferredServerLoc, privateKey.PublicKey().String())

			if tt.expectedErrorMessage != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErrorMessage)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedConfig, gotConfig)
			}

			if tt.mockResponse != nil {
				require.NotNil(t, mockRT.req)
				assert.Equal(t, "application/json", mockRT.req.Header.Get("Content-Type"))
				assert.Equal(t, "no-cache", mockRT.req.Header.Get("Cache-Control"))

				body, err := io.ReadAll(mockRT.req.Body)
				require.NoError(t, err)

				var confReq C.ConfigRequest
				err = json.Unmarshal(body, &confReq)
				require.NoError(t, err)

				assert.Equal(t, common.Platform, confReq.Platform)
				assert.Equal(t, common.Name, confReq.AppName)
				assert.Equal(t, mockUser.DeviceID(), confReq.DeviceID)
				assert.Equal(t, privateKey.PublicKey().String(), confReq.WGPublicKey)
				if tt.preferredServerLoc != nil {
					assert.Equal(t, tt.preferredServerLoc, confReq.PreferredLocation)
				}
			}
		})
	}
}
