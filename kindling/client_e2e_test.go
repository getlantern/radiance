//go:build lantern_e2e

package kindling

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getlantern/radiance/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalHTTPClient(t *testing.T) {
	var outsideRequests atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outsideRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(outside.Close)

	var localRequests atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localRequests.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, outside.URL, http.StatusFound)
			return
		}
		assert.Contains(t, []string{"/v1", "/v0/pro-server"}, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(local.Close)
	t.Setenv("LANTERN_E2E_API_URL", local.URL)
	t.Setenv("HTTP_PROXY", outside.URL)
	t.Setenv("ALL_PROXY", outside.URL)
	client := HTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	for _, endpoint := range []string{common.GetBaseURL(), common.GetProServerURL()} {
		resp, err := client.Get(endpoint)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	}
	for _, endpoint := range []string{outside.URL, local.URL + "/redirect", "https://api.getiantem.org/user-data"} {
		_, err := client.Get(endpoint)
		require.ErrorContains(t, err, "E2E HTTP request must target the configured local API")
	}
	require.EqualValues(t, 3, localRequests.Load())
	require.Zero(t, outsideRequests.Load())
}

func TestLocalHTTPClientRequiresConfiguration(t *testing.T) {
	t.Setenv("LANTERN_E2E_API_URL", "")
	require.Panics(t, func() { HTTPClient() })
	require.Panics(t, Init)
}

func TestLocalHTTPClientClosesRejectedBodies(t *testing.T) {
	t.Setenv("LANTERN_E2E_API_URL", "http://127.0.0.1:8080")
	client := HTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	for _, endpoint := range []string{
		"http://127.0.0.1:8081/v1",
		"https://127.0.0.1:8080/v1",
		"http://user:password@127.0.0.1:8080/v1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			body := &localRequestBody{Reader: strings.NewReader("request")}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, body)
			require.NoError(t, err)
			_, err = client.Do(request)
			require.ErrorContains(t, err, "E2E HTTP request must target the configured local API")
			require.True(t, body.closed)
		})
	}
}

type localRequestBody struct {
	*strings.Reader
	closed bool
}

func (b *localRequestBody) Close() error {
	b.closed = true
	return nil
}
