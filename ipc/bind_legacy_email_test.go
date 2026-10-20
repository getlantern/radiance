package ipc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/backend"
)

type bindLegacyEmailTransport func(*http.Request) (*http.Response, error)

func (f bindLegacyEmailTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestBindLegacyEmailClientRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
		call func(*Client) error
	}{
		{
			name: "start",
			path: "/account/bind-legacy-email/start",
			body: `{"email":"real@example.com"}`,
			call: func(c *Client) error {
				return c.StartBindLegacyEmail(t.Context(), "real@example.com")
			},
		},
		{
			name: "complete",
			path: "/account/bind-legacy-email/complete",
			body: `{"email":"real@example.com","password":"new-password","code":"123456"}`,
			call: func(c *Client) error {
				return c.CompleteBindLegacyEmail(t.Context(), "real@example.com", "new-password", "123456")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
				t.Run(http.StatusText(status), func(t *testing.T) {
					client := &Client{http: &http.Client{
						Transport: bindLegacyEmailTransport(func(r *http.Request) (*http.Response, error) {
							require.Equal(t, http.MethodPost, r.Method)
							require.Equal(t, tc.path, r.URL.Path)
							require.Equal(t, "application/json", r.Header.Get("Content-Type"))
							body, err := io.ReadAll(r.Body)
							require.NoError(t, err)
							require.JSONEq(t, tc.body, string(body))
							return &http.Response{
								StatusCode: status,
								Body:       io.NopCloser(strings.NewReader("binding rejected\n")),
							}, nil
						}),
					}}
					err := tc.call(client)
					if status == http.StatusOK {
						require.NoError(t, err)
					} else {
						var ipcErr *Error
						require.ErrorAs(t, err, &ipcErr)
						require.Equal(t, status, ipcErr.Status)
						require.Equal(t, "binding rejected", ipcErr.Message)
					}
				})
			}
		})
	}
}

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
