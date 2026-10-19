package ipc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getlantern/radiance/backend"
	"github.com/stretchr/testify/require"
)

func TestBindLegacyEmailRoutesRejectInvalidRequests(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	for _, action := range []string{"start", "complete"} {
		request := httptest.NewRequest(http.MethodPost, accountBindLegacyEmailEndpoint+"/"+action, strings.NewReader("{"))
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		require.Equal(t, http.StatusBadRequest, response.Code)
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, accountBindLegacyEmailEndpoint+"/unknown", nil))
	require.Equal(t, http.StatusNotFound, response.Code)
}
