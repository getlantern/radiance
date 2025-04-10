package backend

import (
	"context"
	"crypto/rand"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/user"
	"github.com/getlantern/timezone"
)

const (
	// Required common headers to send to the proxy server.
	appVersionHeader        = "X-Lantern-App-Version"
	versionHeader           = "X-Lantern-Version"
	platformHeader          = "X-Lantern-Platform"
	appNameHeader           = "X-Lantern-App"
	deviceIDHeader          = "X-Lantern-Device-Id"
	userIDHeader            = "X-Lantern-User-Id"
	supportedDataCapsHeader = "X-Lantern-Supported-Data-Caps"
	timeZoneHeader          = "X-Lantern-Time-Zone"
	randomNoiseHeader       = "X-Lantern-Rand"
)

// NewRequestWithHeaders creates a new [http.Request] with the required headers.
func NewRequestWithHeaders(ctx context.Context, method, url string, body io.Reader, user user.BaseUser) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	// add required headers. Currently, all but the auth token are placeholders.
	req.Header.Set(appVersionHeader, app.ClientVersion)
	req.Header.Set(versionHeader, app.Version)
	req.Header.Set(userIDHeader, strconv.FormatInt(user.LegacyID(), 10))
	req.Header.Set(platformHeader, app.Platform)
	req.Header.Set(appNameHeader, app.Name)
	req.Header.Set(deviceIDHeader, user.DeviceID())
	return req, nil
}

// NewIssueRequest creates a new HTTP request with the required headers for issue reporting.
func NewIssueRequest(ctx context.Context, method, url string, body io.Reader, user user.BaseUser) (*http.Request, error) {
	req, err := NewRequestWithHeaders(ctx, method, url, body, user)
	if err != nil {
		return nil, err
	}

	req.Header.Set("content-type", "application/x-protobuf")

	// data caps
	req.Header.Set(supportedDataCapsHeader, "monthly,weekly,daily")

	// time zone
	if tz, err := timezone.IANANameForTime(time.Now()); err == nil {
		req.Header.Set(timeZoneHeader, tz)
	}

	// We include a random length string here to make it harder for censors to identify lantern
	// based on consistent packet lengths.
	req.Header.Add(randomNoiseHeader, randomizedString())

	return req, nil
}

// randomizedString returns a random string to avoid consistent packet lengths censors
// may use to detect Lantern.
func randomizedString() string {
	const charset = "abcdefghijklmnopqrstuvwxyz"
	size, err := rand.Int(rand.Reader, big.NewInt(300))
	if err != nil {
		return ""
	}

	bytes := make([]byte, size.Int64())
	for i := range bytes {
		index, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return ""
		}
		bytes[i] = charset[index.Int64()]
	}
	return string(bytes)
}
