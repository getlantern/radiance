package common

import (
	"time"

	"github.com/getlantern/radiance/common/env"
)

// Version is the application version, injected at build time via ldflags:
//
//	-X 'github.com/getlantern/radiance/common.Version=x.y.z'
var Version = "dev"

const (
	Name = "lantern"

	DefaultHTTPTimeout = (60 * time.Second)

	// API URLs
	ProServerURL      = "https://api.getiantem.org"
	StageProServerURL = "https://api.staging.iantem.io/pro-server"
	BaseURL           = "https://df.iantem.io/api/v1"
	StageBaseURL      = "https://api.staging.iantem.io/v1"
)

func GetVersion() string {
	if v := env.GetString(env.AppVersion); v != "" {
		return v
	}
	return Version
}

func GetProServerURL() string {
	if Stage() {
		return StageProServerURL
	}
	return ProServerURL
}

func GetBaseURL() string {
	if Stage() {
		return StageBaseURL
	}
	return BaseURL
}
