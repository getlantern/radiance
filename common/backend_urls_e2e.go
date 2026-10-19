//go:build lantern_e2e

package common

import (
	"net"
	"net/url"
	"os"
	"strconv"
)

// GetProServerURL returns the local Pro API endpoint, panicking on invalid E2E configuration.
func GetProServerURL() string {
	return localAPIOrigin() + "/v0/pro-server"
}

// GetBaseURL returns the local account API endpoint, panicking on invalid E2E configuration.
func GetBaseURL() string {
	return localAPIOrigin() + "/v1"
}

func localAPIOrigin() string {
	origin := os.Getenv("LANTERN_E2E_API_URL")
	u, err := url.Parse(origin)
	if err == nil && u.Scheme == "http" && u.User == nil && origin == "http://"+u.Host {
		ip := net.ParseIP(u.Hostname())
		port, err := strconv.Atoi(u.Port())
		if ip != nil && ip.IsLoopback() && err == nil && port > 0 && port <= 65535 {
			return origin
		}
	}
	panic("LANTERN_E2E_API_URL must be an HTTP loopback IP origin with an explicit port")
}
