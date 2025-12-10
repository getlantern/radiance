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
)

var AppVersion = Version
