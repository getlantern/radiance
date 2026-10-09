package account

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurlFromRequestPreservesBody(t *testing.T) {
	const body = `{"code":"abc"}`
	req, err := http.NewRequest(http.MethodPost, "https://example.com/users/recovery/complete/email", strings.NewReader(body))
	require.NoError(t, err)

	curl := curlFromRequest(req)
	assert.Contains(t, curl, "-d '"+body+"'", "curl command dropped the body")

	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "request body changed after building the curl command")
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
