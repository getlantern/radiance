package common

import (
	"time"
)

const (
	Name = "lantern"

	// filenames
	LogFileName        = "lantern.log"
	ConfigFileName     = "config.json"
	ServersFileName    = "servers.json"
	DefaultHTTPTimeout = (60 * time.Second)

	// API URLs
	ProServerURL      = "https://api.getiantem.org"
	StageProServerURL = "https://api.staging.iantem.io/pro-server"
	BaseURL           = "https://df.iantem.io/api/v1"
	StageBaseURL      = "https://api.staging.iantem.io/v1"
)

// AppVersion is the application version, injected at build time via ldflags:
//
//	-X 'github.com/getlantern/radiance/common.AppVersion=x.y.z'
var AppVersion = "unknown"

// GetProServerURL returns the pro server URL based on the current environment.
func GetProServerURL() string {
	if Stage() {
		return StageProServerURL
	}
	return ProServerURL
}

// GetBaseURL returns the auth/user base URL based on the current environment.
func GetBaseURL() string {
	if Stage() {
		return StageBaseURL
	}
	return BaseURL
}
