package backend

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/user"
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

// NewRequestWithHeaders creates a new [http.Request] with the required headers.
func NewRequestWithHeaders(ctx context.Context, method, url string, body io.Reader, user *user.User) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	// add required headers. Currently, all but the auth token are placeholders.
	req.Header.Set(appVersionHeader, app.ClientVersion)
	req.Header.Set(versionHeader, app.Version)
	req.Header.Set(userIdHeader, strconv.FormatInt(user.LegacyID(), 10))
	req.Header.Set(platformHeader, app.Platform)
	req.Header.Set(appNameHeader, app.AppName)
	req.Header.Set(deviceIdHeader, user.DeviceID())
	return req, nil
}
