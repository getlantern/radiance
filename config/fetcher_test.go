package config

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/getlantern/radiance/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type mockRoundTripper struct {
	req       *http.Request
	resp      *http.Response
	err       error
	continueC chan struct{}
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.req = req
	select {
	case m.continueC <- struct{}{}:
	default:
	}
	return m.resp, m.err
}

func TestFetcher(t *testing.T) {
	buf, _ := proto.Marshal(testConfigResponse)
	confReader := io.NopCloser(bytes.NewReader(buf))
	tests := []struct {
		name     string
		response *http.Response
		assert   func(*testing.T, *ConfigResponse, error)
	}{
		{
			name:     "received new config",
			response: &http.Response{StatusCode: http.StatusOK, Body: confReader},
			assert: func(t *testing.T, got *ConfigResponse, err error) {
				require.NoError(t, err)
				if !proto.Equal(testConfigResponse, got) {
					// Use Failf to print the expected and actual values nicely.
					require.Failf(t, "Config mismatch",
						"expected: %+v\nactual  : %+v",
						testConfigResponse, got,
					)
				}
			},
		}, {
			name:     "did not receive new config",
			response: &http.Response{StatusCode: http.StatusNoContent},
			assert: func(t *testing.T, got *ConfigResponse, err error) {
				assert.NoError(t, err)
				assert.Nil(t, got)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetcher := newFetcher(&http.Client{
				Transport: &mockRoundTripper{resp: tt.response},
			}, &user.User{})
			got, err := fetcher.fetchConfig(nil)
			tt.assert(t, got, err)
		})
	}
}

func TestFetch_RequiredHeaders(t *testing.T) {
	mockRT := &mockRoundTripper{resp: &http.Response{StatusCode: http.StatusBadRequest}}
	fetcher := newFetcher(&http.Client{
		Transport: mockRT,
	}, &user.User{})
	_, err := fetcher.fetchConfig(nil)
	require.Error(t, err)

	req := mockRT.req
	require.NotNil(t, req, "no request sent")
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)

	cfg := new(ConfigRequest)
	err = proto.Unmarshal(body, cfg)
	require.NoError(t, err)

	ci := cfg.GetClientInfo()
	if assert.NotNil(t, ci, "missing client info") {
		assert.NotEmpty(t, ci.SingboxVersion, "config request missing singbox version")
		assert.NotEmpty(t, ci.ClientVersion, "config request missing client version")
		assert.NotEmpty(t, ci.UserId, "config request missing user id")
	}

	p := cfg.GetProxy()
	assert.NotNil(t, p, "missing proxy info")
}
