//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

const (
	apiURL   = "http://unix"
	sockFile = "radiance.sock"
)

var sockPath = "radiance.sock" // default to current directory

// SetSocketPath sets the path for the Unix domain socket file for client connections.
func SetSocketPath(path string) {
	sockPath = filepath.Join(path, sockFile)
}

func dialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return net.DialUnix("unix", nil, &net.UnixAddr{
		Name: sockPath,
		Net:  "unix",
	})
}

// listen creates a Unix domain socket listener in the specified directory.
func listen(path string) (net.Listener, error) {
	path = filepath.Join(path, sockFile)
	os.Remove(path)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: path,
		Net:  "unix",
	})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	uid, gid := getUidGid(path)
	if err := os.Chown(path, uid, gid); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return listener, nil
}

func getUidGid(sockPath string) (int, int) {
	parentDir := filepath.Dir(sockPath)
	fInfo, err := os.Stat(parentDir)
	if err == nil {
		if stat, ok := fInfo.Sys().(*syscall.Stat_t); ok {
			return int(stat.Uid), int(stat.Gid)
		}
	}
	return os.Getuid(), os.Getgid()
}
