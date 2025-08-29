//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/getlantern/radiance/common"
)

const (
	apiURL = "http://unix"
)

var (
	sockFile = "radiance.sock"
	uid      = os.Getuid()
	gid      = os.Getgid()
)

func dialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return net.DialUnix("unix", nil, &net.UnixAddr{
		Name: filepath.Join(common.DataPath(), sockFile),
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
	if err := os.Chown(path, uid, gid); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return listener, nil
}
