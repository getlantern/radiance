package common

import "runtime"

func IsAndroid() bool {
	return runtime.GOOS == "android"
}

func IsIOS() bool {
	return runtime.GOOS == "ios"
}

func IsMacOS() bool {
	return runtime.GOOS == "darwin"
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}
