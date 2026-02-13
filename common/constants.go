package common

import (
	"runtime"
	"time"
)

const (
	Name    = "lantern"
	Version = "9.0.1"

	Platform = runtime.GOOS

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

var AppVersion = Version

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
