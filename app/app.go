package app

import "runtime"

const (
	Name = "lantern"

	// Placeholders to use in the request headers.
	ClientVersion = "7.6.47"
	Version       = "7.6.47"

	Platform = runtime.GOOS

	// filenames
	LogFileName = "lantern.log"
)
