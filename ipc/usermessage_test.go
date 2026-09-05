package ipc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/backend"
)

func TestUserMessageRoutes(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)

	response := serveUserMessageRequest(t, api, http.MethodGet, userMessageEndpoint, nil)
	require.Equal(t, http.StatusOK, response.Code)
	var current CurrentUserMessageResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &current))
	require.Nil(t, current.Message)

	response = serveUserMessageRequest(t, api, http.MethodPost, userMessageRefreshEndpoint, nil)
	require.Equal(t, http.StatusNoContent, response.Code)

	response = serveUserMessageRequest(t, api, http.MethodPatch, userMessageActivityEndpoint,
		UserMessageActivityRequest{Active: true})
	require.Equal(t, http.StatusNoContent, response.Code)

	response = serveUserMessageRequest(t, api, http.MethodPost, userMessageAcknowledgeEndpoint,
		UserMessageAcknowledgeRequest{DisplayID: "not-pending"})
	require.Equal(t, http.StatusConflict, response.Code)
}

func serveUserMessageRequest(
	t *testing.T,
	api http.Handler,
	method string,
	endpoint string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&encoded).Encode(body))
	}
	request := httptest.NewRequest(method, endpoint, &encoded)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	return response
}
