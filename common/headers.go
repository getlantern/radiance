package common

import (
	"context"
	"crypto/rand"
	"io"
	"math/big"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/getlantern/timezone"

	"github.com/getlantern/radiance/common/settings"
)

// publicIP holds the detected public IP address, set once at startup.
var publicIP atomic.Value // string

func init() {
	publicIP.Store("") // ensure publicIP is type string
}

// SetPublicIP stores the detected public IP for inclusion in API requests. It should only be called
// once at startup after successfully detecting the public IP.
func SetPublicIP(ip string) {
	publicIP.Store(ip)
}

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
	ClientIPHeader          = "X-Lantern-Config-Client-IP"
	ContentTypeHeader       = "content-type"
	AcceptHeader            = "accept"
)

// NewRequestWithHeaders creates a new [http.Request] with the required headers.
func NewRequestWithHeaders(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	// We include a random length string here to make it harder for censors to identify lantern
	// based on consistent packet lengths.
	req.Header.Add(RandomNoiseHeader, randomizedString())

	req.Header.Set(AppVersionHeader, Version)
	req.Header.Set(VersionHeader, Version)
	req.Header.Set(UserIDHeader, settings.GetString(settings.UserIDKey))
	req.Header.Set(PlatformHeader, Platform)
	req.Header.Set(AppNameHeader, Name)
	req.Header.Set(DeviceIDHeader, settings.GetString(settings.DeviceIDKey))
	if tz, err := timezone.IANANameForTime(time.Now()); err == nil {
		req.Header.Set(TimeZoneHeader, tz)
	}
	if ip := publicIP.Load().(string); ip != "" {
		req.Header.Set(ClientIPHeader, ip)
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
