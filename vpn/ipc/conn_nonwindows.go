//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
)

const apiURL = "http://lantern"

func dialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return net.DialUnix("unix", nil, &net.UnixAddr{
		Name: socketPath(),
		Net:  "unix",
	})
}

type sockListener struct {
	net.Listener
	path string
}

func listen() (net.Listener, error) {
	path := socketPath()
	os.Remove(path)

	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0755)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: path,
		Net:  "unix",
	})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := setPermissions(); err != nil {
		listener.Close()
		return nil, fmt.Errorf("update socket permissions: %w", err)
	}
	socket := &sockListener{
		Listener: listener,
		path:     path,
	}
	// ensure listener is closed
	runtime.AddCleanup(socket, func(ll *net.UnixListener) {
		ll.Close()
	}, listener)
	return socket, nil
}

func (l *sockListener) Close() error {
	err := l.Listener.Close()
	os.Remove(l.path)
	return err
}

func getConnPeer(conn net.Conn) (p usr, err error) {
	uconn, ok := conn.(*net.UnixConn)
	if !ok {
		return p, fmt.Errorf("not a unix domain socket connection")
	}
	rawConn, err := uconn.SyscallConn()
	if err != nil {
		return p, fmt.Errorf("syscall conn: %w", err)
	}

	uid, err := getUid(rawConn)
	if err != nil {
		return p, fmt.Errorf("get uid: %w", err)
	}
	if uid == 0 {
		return usr{
			uid:     "0",
			uname:   "root",
			isAdmin: true,
		}, nil
	}

	uidStr := strconv.FormatUint(uint64(uid), 10)
	peer, err := getPeerUser(uid, uidStr)
	if err != nil {
		return p, err
	}
	return peer, nil
}

func linuxUserInControlGroup(u *user.User) (bool, error) {
	group, err := user.LookupGroup(controlGroup)
	if err != nil {
		return false, fmt.Errorf("lookup %s group: %w", controlGroup, err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return false, fmt.Errorf("lookup groups for %s: %w", u.Username, err)
	}
	for _, gid := range gids {
		if gid == group.Gid {
			return true, nil
		}
	}
	return false, nil
}

func getPeerUser(uid uint32, uidStr string) (usr, error) {
	u, err := user.LookupId(uidStr)
	if err != nil {
		return usr{}, fmt.Errorf("lookup user id %v: %w", uid, err)
	}

	if runtime.GOOS == "linux" {
		inControlGroup, err := linuxUserInControlGroup(u)
		if err != nil {
			return usr{}, err
		}
		return usr{
			uid:            uidStr,
			uname:          u.Username,
			inControlGroup: inControlGroup,
		}, nil
	}

	return usr{
		uid:     uidStr,
		uname:   u.Username,
		isAdmin: canSudo(u.Username),
	}, nil
}
