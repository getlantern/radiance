package app

import "runtime"

const (
	Name = "lantern"

	// Placeholders to use in the request headers.
	ClientVersion = "9.0.0"
	Version       = "9.0.0"

	Platform = runtime.GOOS

	// filenames
	LogFileName    = "lantern.log"
	ConfigFileName = "config.json"
)
