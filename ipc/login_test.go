package ipc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/backend"
)

type loginTransport func(*http.Request) (*http.Response, error)

func (f loginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoginClientAccountGuard(t *testing.T) {
	for _, expectedID := range []int64{0, 123} {
		t.Run(strconv.FormatInt(expectedID, 10), func(t *testing.T) {
			client := &Client{http: &http.Client{Transport: loginTransport(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, http.MethodPost, r.Method)
				path := accountLoginEndpoint
				if expectedID != 0 {
					path += "/current"
				}
				require.Equal(t, path, r.URL.Path)
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				var req LoginRequest
				require.NoError(t, json.Unmarshal(body, &req))
				require.Equal(t, expectedID, req.ExpectedLegacyID)
				require.Equal(t, "test@example.com", req.Email)
				require.Equal(t, "password", req.Password)
				if expectedID == 0 {
					require.NotContains(t, string(body), "expectedLegacyID")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"legacyID":123}`))}, nil
			})}}
			var user *account.UserData
			var err error
			if expectedID == 0 {
				user, err = client.Login(t.Context(), "test@example.com", "password")
			} else {
				user, err = client.LoginForAccount(t.Context(), "test@example.com", "password", expectedID)
			}
			require.NoError(t, err)
			require.Equal(t, int64(123), user.LegacyID)
		})
	}
}

func TestLoginRouteRejectsInvalidAccountGuard(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	for _, body := range []string{`{"expectedLegacyID":-1}`, `{"expectedLegacyID":0}`, `{}`} {
		request := httptest.NewRequest(http.MethodPost, accountLoginEndpoint+"/current", strings.NewReader(body))
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Contains(t, response.Body.String(), "expected account ID must be positive")
	}
}

func TestLoginForAccountRejectsOldDaemon(t *testing.T) {
	oldDaemon := http.NewServeMux()
	oldDaemon.HandleFunc("POST "+accountLoginEndpoint, func(http.ResponseWriter, *http.Request) {
		t.Error("guarded login reached the unguarded route")
	})
	client := &Client{http: &http.Client{Transport: loginTransport(func(r *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		oldDaemon.ServeHTTP(response, r)
		return response.Result(), nil
	})}}
	_, err := client.LoginForAccount(t.Context(), "test@example.com", "password", 123)
	var ipcErr *Error
	require.ErrorAs(t, err, &ipcErr)
	require.Equal(t, http.StatusNotFound, ipcErr.Status)
}
