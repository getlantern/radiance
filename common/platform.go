package common

import "runtime"

func IsAndoid() bool {
	return runtime.GOOS == "android"
}

func IsIOS() bool {
	return runtime.GOOS == "ios"
}
