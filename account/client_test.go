package account

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
)

func TestCurlFromRequestRedactsSecretHeaders(t *testing.T) {
	const token = "pro-token-value"
	req, err := http.NewRequest(http.MethodGet, "https://example.com/user-data", nil)
	require.NoError(t, err)
	req.Header.Set(common.ProTokenHeader, token)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Cookie", "session="+token)
	req.Header.Set(common.DeviceIDHeader, "device-id-value")

	curl := curlFromRequest(req)
	assert.NotContains(t, curl, token, "credential leaked into curl command")
	assert.Contains(t, curl, "device-id-value", "redaction dropped a non-secret header")
}

func TestCurlFromRequestRedactsQueryValues(t *testing.T) {
	const email = "user@example.com"
	req, err := http.NewRequest(http.MethodGet, "https://example.com/users/salt?email="+url.QueryEscape(email), nil)
	require.NoError(t, err)
	original := req.URL.String()

	curl := curlFromRequest(req)
	assert.NotContains(t, curl, url.QueryEscape(email), "query value leaked into curl command")
	assert.Contains(t, curl, "email=redacted", "curl command dropped the query key")
	assert.Equal(t, original, req.URL.String(), "request URL changed after building the curl command")
}

func TestCurlFromRequestOmitsBody(t *testing.T) {
	const recoveryCode = "recovery-code-value"
	req, err := http.NewRequest(http.MethodPost, "https://example.com/users/recovery/complete/email", strings.NewReader(recoveryCode))
	require.NoError(t, err)

	curl := curlFromRequest(req)
	assert.NotContains(t, curl, recoveryCode, "request body leaked into curl command")
	assert.Contains(t, curl, "-d '<redacted>'", "curl command dropped the body marker")

	// curlFromRequest must leave the body readable for the actual send.
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, recoveryCode, string(body), "request body changed after building the curl command")
}

func TestErrorResponseBodyOmittedFromLog(t *testing.T) {
	const echoed = "recovery-code-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid_code: "+echoed, http.StatusBadRequest)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	a := &Client{
		httpClient: srv.Client(),
		logger:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	_, err := a.sendRequest(context.Background(), http.MethodPost, srv.URL+"/users/recovery/validate/email", nil, nil, nil)
	require.Error(t, err)
	assert.NotContains(t, buf.String(), echoed, "echoed response body reached the log")
	assert.Contains(t, buf.String(), "status=400", "error log dropped the status")
	assert.Contains(t, err.Error(), "invalid_code", "returned error dropped the server error code")
}
