package common

import "runtime"

// Platform is the runtime platform string, defaulting to runtime.GOOS but
// overridable via RADIANCE_PLATFORM (handled in common.Init) for QA scenarios
// that need to impersonate a different platform — e.g. running radiance as a
// Go process on macOS while making the API see us as an Android client.
var Platform = runtime.GOOS

func IsAndroid() bool {
	return Platform == "android"
}

func IsIOS() bool {
	return Platform == "ios"
}

func IsMacOS() bool {
	return Platform == "darwin"
}

func IsWindows() bool {
	return Platform == "windows"
}

func IsMobile() bool {
	return IsAndroid() || IsIOS()
}
