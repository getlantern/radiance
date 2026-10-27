package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/helper"
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

func TestHelperRelayEndpointRejectsAMalformedBody(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	r := httptest.NewRequest(http.MethodPost, helperRelayEndpoint, strings.NewReader(`{"op":`))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "bad helper relay request\n", w.Body.String())
}

// localTransport serves a Client's requests from api in-process.
type localTransport struct{ api *localapi }

func (t localTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.api.ServeHTTP(w, r)
	return w.Result(), nil
}

// A relayed request goes from ipc.Client through the endpoint and the
// backend's relayer to lantern-cloud, and lantern-cloud's answer, success or
// not, comes back as a RelayResult.
func TestHelperRelayClientRoundTrip(t *testing.T) {
	var gotPath, gotBody string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Path == "/v1/helper/invites/claim" {
			http.Error(w, "invite already claimed or expired", http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte(`{"pairings":[]}`))
	}))
	defer cloud.Close()
	b := &backend.LocalBackend{}
	b.SetHelperRelayForTest(helper.NewRelayer(cloud.Client(), cloud.URL+"/v1", nil))
	c := newClient()
	c.http.Transport = localTransport{api: newLocalAPI(b, false)}

	res, err := c.HelperRelay(context.Background(), "pairings", []byte(`{"x":1}`))
	require.NoError(t, err)
	assert.Equal(t, "/v1/helper/pairings", gotPath)
	assert.JSONEq(t, `{"x":1}`, gotBody)
	assert.Equal(t, http.StatusOK, res.Status)
	assert.JSONEq(t, `{"pairings":[]}`, string(res.Body))
	assert.Empty(t, res.Error)

	res, err = c.HelperRelay(context.Background(), "invites/claim", []byte(`{}`))
	require.NoError(t, err, "lantern-cloud's error is a result, not a relay failure")
	assert.Equal(t, http.StatusConflict, res.Status)
	assert.Equal(t, "invite already claimed or expired", res.Error)
	assert.Nil(t, res.Body)

	_, err = c.HelperRelay(context.Background(), "peer/register", nil)
	var ipcErr *Error
	require.ErrorAs(t, err, &ipcErr)
	assert.Equal(t, http.StatusBadRequest, ipcErr.Status)
}
