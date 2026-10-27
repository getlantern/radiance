package ipc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/backend"
)

func serveHelper(t *testing.T, api *localapi, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	r := httptest.NewRequest(method, path, &buf)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	return w
}

func TestHelperRelayEndpoint(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	w := serveHelper(t, api, http.MethodPost, helperRelayEndpoint, HelperRelayRequest{Op: "peer/register"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "unknown helper operation\n", w.Body.String())

	w = serveHelper(t, api, http.MethodPost, helperRelayEndpoint, HelperRelayRequest{Op: "pairings"})
	assert.Equal(t, http.StatusBadGateway, w.Code, "a backend without a relayer cannot reach lantern-cloud")
	assert.Equal(t, "helper relay failed\n", w.Body.String(), "the error is fixed text")

	w = serveHelper(t, api, http.MethodGet, helperRelayEndpoint, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
