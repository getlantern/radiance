//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

const (
	apiURL = "http://unix"
)

var (
	// might need a locker
	socksPath = ""
	sockFile  = "radiance.sock"
	uid       = os.Getuid()
	gid       = os.Getgid()
)

func dialContext(_ context.Context, _, _ string) (net.Conn, error) {
	if socksPath == "" {
		return nil, fmt.Errorf("socks path not defined")
	}
	return net.DialUnix("unix", nil, &net.UnixAddr{
		Name: socksPath,
		Net:  "unix",
	})
}

// listen creates a Unix domain socket listener in the specified directory.
func listen(path string) (net.Listener, error) {
	socksPath = filepath.Join(path, sockFile)
	os.Remove(socksPath)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: socksPath,
		Net:  "unix",
	})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return listener, nil
}
