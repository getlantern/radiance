//go:build !android && !ios && !windows

package ipc

import (
	"fmt"
	"os/user"
	"strconv"
)

func controlGroupInfo() (*user.Group, error) {
	group, err := user.LookupGroup(controlGroup)
	if err != nil {
		return nil, fmt.Errorf("lookup %s group: %w", controlGroup, err)
	}
	return group, nil
}

func controlGroupGID() (string, error) {
	group, err := controlGroupInfo()
	if err != nil {
		return "", err
	}
	return group.Gid, nil
}

func controlGroupGIDInt() (int, error) {
	gid, err := controlGroupGID()
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(gid)
	if err != nil {
		return 0, fmt.Errorf("convert %s gid %s: %w", controlGroup, gid, err)
	}
	return parsed, nil
}

