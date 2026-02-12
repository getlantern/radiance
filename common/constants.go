package common

import (
	"time"
)

const (
	Name    = "lantern"
	Version = "9.0.1"

	// filenames
	LogFileName        = "lantern.log"
	ConfigFileName     = "config.json"
	ServersFileName    = "servers.json"
	DefaultHTTPTimeout = (60 * time.Second)
)

var AppVersion = Version
