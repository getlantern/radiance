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

func TestErrorResponseBodyRedactedFromError(t *testing.T) {
	const email = "someone@example.com"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Invalid user-id: 42 and email "+email, http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := &Client{httpClient: srv.Client(), logger: slog.New(slog.DiscardHandler)}
	_, err := a.sendRequest(context.Background(), http.MethodPost, srv.URL+"/purchase", nil, nil, nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), email, "echoed email reached the returned error")
	assert.Contains(t, err.Error(), "unexpected status 500 body Invalid user-id: 42 and email [redacted]")
}

func TestRedactResponseBody(t *testing.T) {
	removed := map[string]string{
		"email":    "no account for jane.doe+vpn@example.co.uk",
		"code":     "wrong code 042817",
		"jwt":      "bad token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl",
		"token":    "unknown pro token 9f8e7d6c5b4a39281706f5e4d3c2b1a0",
		"deviceID": "device 3f2504e0-4f89-11d3-9a0c-0305e82c3301 not linked",
	}
	for name, body := range removed {
		t.Run(name, func(t *testing.T) {
			got := string(redactResponseBody([]byte(body)))
			assert.Contains(t, got, redacted)
			for field := range strings.FieldsSeq(body) {
				if strings.ContainsAny(field, "@0123456789") {
					assert.NotContains(t, got, field)
				}
			}
		})
	}

	// Server error strings the app matches on must survive.
	kept := []string{
		"invalid_code",
		"user_not_found",
		"recovery_not_found",
		"wrong-link-code",
		"we_are_experiencing_technical_difficulties",
		"wrong-reseller-code",
		"missing verifier or salt ",
		"user already exists",
		"user with this legacy user ID already exists",
		"purchase_not_found",
		"user with provided email not found",
		"no valid purchases for user",
		"err_while_sending_code",
		"error-wrong-code",
		"<error-email-not-verified>",
		"error restoring purchase",
		"Invalid referral",
		"invalid-referral-code",
		"Cannot use your own code for promotion",
		"bad-input-missing-device-id",
	}
	for _, body := range kept {
		assert.Equal(t, body, string(redactResponseBody([]byte(body))))
	}
}
