package app

import "runtime"

const (
	AppName = "radiance"

	// Placeholders to use in the request headers.
	ClientVersion = "7.6.47"
	Version       = "7.6.47"

	Platform = runtime.GOOS

	// TODO: this should be a platform specific path to the log directory where we save all logs
	LogDir = "logs"
)
