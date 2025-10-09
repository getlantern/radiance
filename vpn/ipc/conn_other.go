//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/getlantern/radiance/internal"
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

type sockListener struct {
	net.Listener
	path string
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
	uid, gid := getNonRootOwner(path)
	if err := os.Chown(path, uid, gid); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chown %s: %w", path, err)
	}
	return &sockListener{
		Listener: listener,
		path:     path,
	}, nil
}

func (l *sockListener) Close() error {
	err := l.Listener.Close()
	os.Remove(l.path)
	return err
}

func getNonRootOwner(path string) (uid, gid int) {
	uid = os.Getuid()
	gid = os.Getgid()
	if uid != 0 {
		return uid, gid
	}

	slog.Log(context.Background(), internal.LevelTrace, "searching for non-root owner of", "path", path)
	for {
		parentDir := filepath.Dir(path)
		if parentDir == path || parentDir == "/" {
			break
		}
		path = parentDir

		fInfo, err := os.Stat(path)
		if err != nil {
			slog.Log(context.Background(), internal.LevelTrace, "stat error", "path", path, "error", err)
			continue
		}
		stat, ok := fInfo.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if int(stat.Uid) != 0 {
			slog.Log(context.Background(), internal.LevelTrace, "found non-root owner", "path", path, "uid", stat.Uid, "gid", stat.Gid)
			return int(stat.Uid), int(stat.Gid)
		}
	}
	if slog.Default().Enabled(context.Background(), internal.LevelTrace) {
		slog.Warn("falling back to root owner for", "path", path)
	}
	return uid, gid
}
