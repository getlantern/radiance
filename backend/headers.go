package backend

import (
	"context"
	"crypto/rand"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/getlantern/timezone"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
)

const (
	// Required common headers to send to the proxy server.
	AppVersionHeader        = "X-Lantern-App-Version"
	VersionHeader           = "X-Lantern-Version"
	PlatformHeader          = "X-Lantern-Platform"
	AppNameHeader           = "X-Lantern-App"
	DeviceIDHeader          = "X-Lantern-Device-Id"
	UserIDHeader            = "X-Lantern-User-Id"
	SupportedDataCapsHeader = "X-Lantern-Supported-Data-Caps"
	TimeZoneHeader          = "X-Lantern-Time-Zone"
	RandomNoiseHeader       = "X-Lantern-Rand"
	ProTokenHeader          = "X-Lantern-Pro-Token"
	RefererHeader           = "referer"
	ClientCountryHeader     = "X-Lantern-Client-Country"
	ContentTypeHeader       = "content-type"
	AcceptHeader            = "accept"

)

// NewRequestWithHeaders creates a new [http.Request] with the required headers.
func NewRequestWithHeaders(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Lantern/"+common.AppVersion)
	// We include a random length string here to make it harder for censors to identify lantern
	// based on consistent packet lengths.
	req.Header.Add(RandomNoiseHeader, randomizedString())

	req.Header.Set(AppVersionHeader, common.AppVersion)
	req.Header.Set(VersionHeader, common.Version)
	req.Header.Set(UserIDHeader, strconv.FormatInt(settings.GetInt64(settings.UserIDKey), 10))
	req.Header.Set(PlatformHeader, common.Platform)
	req.Header.Set(AppNameHeader, common.Name)
	req.Header.Set(DeviceIDHeader, settings.GetString(settings.DeviceIDKey))
	return req, nil
}

// NewIssueRequest creates a new HTTP request with the required headers for issue reporting.
func NewIssueRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := NewRequestWithHeaders(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("content-type", "application/x-protobuf")

	// data caps
	req.Header.Set(SupportedDataCapsHeader, "monthly,weekly,daily")

	// time zone
	if tz, err := timezone.IANANameForTime(time.Now()); err == nil {
		req.Header.Set(TimeZoneHeader, tz)
	}

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
