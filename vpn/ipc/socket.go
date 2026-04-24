//go:build !android && !ios && !windows

package ipc

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
)

const controlGroup = "lantern"

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
	path := socketPath()
	if runtime.GOOS == "linux" {
		if _testing || os.Geteuid() != 0 {
			if err := os.Chmod(path, 0600); err != nil {
				return fmt.Errorf("chmod %s: %w", path, err)
			}
			return nil
		}

		gid, err := controlGroupGIDInt()
		if err != nil {
			return err
		}
		if err := os.Chown(path, 0, gid); err != nil {
			return fmt.Errorf("chown %s: %w", path, err)
		}
		if err := os.Chmod(path, 0660); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
		return nil
	}

	// chown admin group and let the OS restrict access
	group, err := user.LookupGroup("admin")
	if err != nil {
		return fmt.Errorf("lookup admin group: %w", err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("convert admin gid %s: %w", group.Gid, err)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
