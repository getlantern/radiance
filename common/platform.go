package common

import "runtime"

func IsAndroid() bool {
	return runtime.GOOS == "android"
}

func IsIOS() bool {
	return runtime.GOOS == "ios"
}
