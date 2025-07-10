package common

import "runtime"

const (
	Name = "lantern"

	// Placeholders to use in the request headers.
	ClientVersion = "0.0.1"
	Version       = "0.0.1"

	Platform = runtime.GOOS

	// filenames
	LogFileName     = "lantern.log"
	ConfigFileName  = "config.json"
	ServersFileName = "servers.json"
)
