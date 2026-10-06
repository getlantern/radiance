package servers

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactingLoggerStripsAccessToken(t *testing.T) {
	const token = "super-secret-token"
	u, err := url.Parse("https://192.0.2.1:8080/api/v1/connect-config?token=" + token)
	require.NoError(t, err)

	tests := []struct {
		name  string
		value any
	}{
		{"redacted url", u.String()},
		{"request description", fmt.Sprintf("GET %s (status: 500)", u)},
		{"url error", &url.Error{Op: "Get", URL: u.String(), Err: fmt.Errorf("connection refused")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := redactingLogger{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			logger.Debug("performing request", "url", tt.value)
			assert.NotContains(t, buf.String(), token, "access token leaked into log")
			assert.Contains(t, buf.String(), "connect-config", "redaction dropped the request path")
		})
	}
}

func TestPrivateServerErrorOmitsAccessToken(t *testing.T) {
	const accessToken = "SECRET-ACCESS-TOKEN"

	// Bind then release a port so the request fails at dial with a *url.Error,
	// which quotes the request URL.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	m := testManager(t)
	m.httpClient = &http.Client{Timeout: 5 * time.Second}

	_, err = m.InviteToPrivateServer("127.0.0.1", port, accessToken, "invite1")
	require.Error(t, err, "expected a dial failure")
	assert.NotContains(t, err.Error(), accessToken, "access token leaked through the returned error")
	assert.Contains(t, err.Error(), "share-link", "redaction removed the request path, leaving an undiagnosable error")
}
