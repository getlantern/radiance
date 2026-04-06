//go:build !android && !ios && !windows

package ipc

import (
	"os"
)

// use a var so it can be overridden in tests
var _socketPath = "/var/run/lantern/lanternd.sock"

// setSocketPathForTesting is only used for testing.
func setSocketPathForTesting(path string) {
	_socketPath = path
}

func socketPath() string {
	return _socketPath
}

func setPermissions() error {
	// we'll check if user is sudoer to restrict access
	return os.Chmod(socketPath(), 0666)
}
