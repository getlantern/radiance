package config

import (
	"bytes"
	"context"

	"io"
	"net/http"
	"strconv"
	"testing"

	C "github.com/getlantern/common"
	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
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
	user.BaseUser
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

func TestFetchConfig(t *testing.T) {
	mockUser := &mockUser{}

	tests := []struct {
		name                 string
		preferredServerLoc   *C.ServerLocation
		mockResponse         *http.Response
		mockError            error
		expectedConfig       *C.ConfigResponse
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
					resp := &C.ConfigResponse{
						UserInfo: C.UserInfo{
							ProToken: "mock-token",
						},
						Options: O.Options{
							RawMessage: json.RawMessage(`{}`),
						},
					}
					data, _ := json.Marshal(resp)
					return data
				}())),
			},
			expectedConfig: &C.ConfigResponse{
				UserInfo: C.UserInfo{
					ProToken: "mock-token",
				},
				Options: O.Options{
					RawMessage: json.RawMessage(`{}`),
				},
			},
		},
		{
			name: "no new config available",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusNoContent,
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
		{
			name: "invalid response body",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader([]byte("invalid-json"))),
			},
			expectedErrorMessage: "unmarshal config response: invalid character 'i' looking for beginning of value",
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
			}, mockUser)

			gotConfig, err := fetcher.fetchConfig(tt.preferredServerLoc)

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

				assert.Equal(t, app.ClientVersion, confReq.ClientVersion)
				assert.Equal(t, strconv.FormatInt(mockUser.LegacyID(), 10), confReq.UserID)
				assert.Equal(t, app.Platform, confReq.OS)
				assert.Equal(t, app.Name, confReq.AppName)
				assert.Equal(t, mockUser.DeviceID(), confReq.DeviceID)
				if tt.preferredServerLoc != nil {
					assert.Equal(t, *tt.preferredServerLoc, confReq.PreferredLocation)
				}
			}
		})
	}
}
