package common

import (
	"context"
	"io"
	"net/http"
)

const (
	// Required common headers to send to the proxy server.
	appVersionHeader = "X-Lantern-App-Version"
	versionHeader    = "X-Lantern-Version"
	platformHeader   = "X-Lantern-Platform"
	appNameHeader    = "X-Lantern-App"
	deviceIdHeader   = "X-Lantern-Device-Id"
	userIdHeader     = "X-Lantern-User-Id"
)

var (
	// Placeholders to use in the request headers.
	ClientVersion = "7.6.47"
	Version       = "7.6.47"
	// userId and proToken will be set to actual values when user management is implemented.
	UserId   = "23409" // set to specific value so the server returns a desired config.
	ProToken = ""
)

// NewRequestWithHeaders creates a new [http.Request] with the required headers.
func NewRequestWithHeaders(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	// add required headers. Currently, all but the auth token are placeholders.
	req.Header.Set(appVersionHeader, ClientVersion)
	req.Header.Set(versionHeader, Version)
	req.Header.Set(userIdHeader, UserId)
	req.Header.Set(platformHeader, "linux")
	req.Header.Set(appNameHeader, "radiance")
	req.Header.Set(deviceIdHeader, "some-uuid-here")
	return req, nil
}
