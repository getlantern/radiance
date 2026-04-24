//go:build android || ios

package ipc

import "fmt"

func controlGroupGID() (string, error) {
	return "", fmt.Errorf("control group lookup is unsupported on %s", controlGroup)
}

func controlGroupGIDInt() (int, error) {
	return 0, fmt.Errorf("control group gid conversion is unsupported on %s", controlGroup)
}
