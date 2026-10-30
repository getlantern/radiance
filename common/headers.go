package common

import (
	"context"
	"crypto/rand"
	"io"
	"math/big"
	"net/http"
	"sync/atomic"

	"github.com/getlantern/radiance/common/settings"
)

type clientIP struct {
	ip string
	// detected is false for an IP restored from storage, which is truncated for privacy and so
	// only good for geolocation.
	detected bool
	// network counts calls to [ForgetDetectedPublicIP].
	network uint64
}

var publicIP atomic.Pointer[clientIP]

func init() {
	publicIP.Store(&clientIP{})
}

// SetPublicIP stores the full public IP detected this session for inclusion in API requests.
func SetPublicIP(ip string) {
	for {
		cur := publicIP.Load()
		if publicIP.CompareAndSwap(cur, &clientIP{ip: ip, detected: ip != "", network: cur.network}) {
			return
		}
	}
}

// CurrentNetwork returns a token, for [SetPublicIPOn], that changes whenever
// [ForgetDetectedPublicIP] runs.
func CurrentNetwork() uint64 {
	return publicIP.Load().network
}

// SetPublicIPOn is [SetPublicIP] for an IP looked up after [CurrentNetwork] returned network. It
// discards ip and reports false if [ForgetDetectedPublicIP] ran since, as the lookup may have
// returned the old network's address.
func SetPublicIPOn(network uint64, ip string) bool {
	for {
		cur := publicIP.Load()
		if cur.network != network {
			return false
		}
		if publicIP.CompareAndSwap(cur, &clientIP{ip: ip, detected: ip != "", network: network}) {
			return true
		}
	}
}

// SetStoredPublicIP stores a truncated public IP from a previous session. API requests carry it
// for geolocation until [SetPublicIP] replaces it, but [SetDetectedClientIP] never sends it. An IP
// already detected this session is kept: a backend restarted within the process (mobile, when IPC
// comes and goes) reloads the stored value after the first backend detected the current one.
func SetStoredPublicIP(ip string) {
	for {
		cur := publicIP.Load()
		if cur.detected || publicIP.CompareAndSwap(cur, &clientIP{ip: ip, network: cur.network}) {
			return
		}
	}
}

// ForgetDetectedPublicIP marks the current public IP as no longer detected, because the network
// changed and the address may have with it. Requests keep carrying it for geolocation, but
// [SetDetectedClientIP] stops sending it until [SetPublicIP] or [SetPublicIPOn] records a new
// detection.
func ForgetDetectedPublicIP() {
	for {
		cur := publicIP.Load()
		if publicIP.CompareAndSwap(cur, &clientIP{ip: cur.ip, network: cur.network + 1}) {
			return
		}
	}
}

// SetDetectedClientIP sets [ClientIPHeader] on req to the public IP detected this session, or
// removes the header if there is none. The server trusts the header as the client's address, so
// requests that need the exact address, rather than its location, must use this.
func SetDetectedClientIP(req *http.Request) {
	if cur := publicIP.Load(); cur.detected {
		req.Header.Set(ClientIPHeader, cur.ip)
	} else {
		req.Header.Del(ClientIPHeader)
	}
}

const (
	// Common Lantern headers. All are request headers sent to the proxy server
	// except OriginHeader, which the server sets on responses.
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

	// OriginHeader is set by Lantern origins on every response. Fronted requests
	// read it to tell an origin-issued status from a CDN edge's, which is
	// otherwise impossible: a CDN relays the origin's response verbatim.
	OriginHeader = "X-Lantern-Origin"
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

	req.Header.Set(AppVersionHeader, GetVersion())
	req.Header.Set(VersionHeader, GetVersion())
	req.Header.Set(UserIDHeader, settings.GetString(settings.UserIDKey))
	req.Header.Set(PlatformHeader, Platform)
	req.Header.Set(AppNameHeader, Name)
	req.Header.Set(DeviceIDHeader, settings.GetString(settings.DeviceIDKey))
	if tz := LocalTimeZone(); tz != "" {
		req.Header.Set(TimeZoneHeader, tz)
	}
	if ip := publicIP.Load().ip; ip != "" {
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
