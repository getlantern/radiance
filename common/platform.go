package common

import "runtime"

func IsAndoid() bool {
	return runtime.GOOS == "android"
}

func IsIOS() bool {
	return runtime.GOOS == "ios"
}
func IsWindows() bool {
	return runtime.GOOS == "windows"
}
func IsLinux() bool {
	return runtime.GOOS == "linux"
}
func IsMac() bool {
	return runtime.GOOS == "darwin"
}
