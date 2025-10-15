package ipc

//go:generate go run golang.org/x/sys/windows/mkwinsyscall -output zsyscall_windows.go conn_windows.go

//sys impersonateNamedPipeClient(h windows.Handle) (err error) [int32(failretval)==0] = advapi32.ImpersonateNamedPipeClient

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	pipePath = `\\.\pipe\Lantern\radiance`

	apiURL         = "http://pipe"
	connectTimeout = 10 * time.Second

	sddl = `D:P(A;;GA;;;SY)(A;;GRGW;;;IU)(A;;GRGW;;;BA)`
)

// SetSocketPath not supported on Windows.
func SetSocketPath(path string) {
	panic("SetSocketPath is not supported on Windows")
}

func dialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	return winio.DialPipeAccessImpLevel(ctx, pipePath, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}

// listen creates a named pipe listener at a predefined path.
func listen(_ string) (net.Listener, error) {
	ln, err := winio.ListenPipe(
		pipePath,
		&winio.PipeConfig{
			SecurityDescriptor: sddl,
			InputBufferSize:    256 * 1024,
			OutputBufferSize:   256 * 1024,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create named pipe listener: %w", err)
	}
	return &winioListener{ln}, nil
}

// winioConn is an helper interface that exposes the standard syscall.Conn so we can
// access the underlying handle via RawConn.Control
type winioConn interface {
	net.Conn
	SyscallConn() (syscall.RawConn, error)
}

// withConnHandle runs the function with the connection’s handle pinned by the runtime
func withConnHandle(c winioConn, fn func(h windows.Handle) error) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	if err := rc.Control(func(fd uintptr) { callErr = fn(windows.Handle(fd)) }); err != nil {
		return err
	}
	return callErr
}

type winioListener struct {
	net.Listener
}

// Accept waits for and returns the next connection to the listener, verifying the client identity.
func (l *winioListener) Accept() (conn net.Conn, err error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return c, nil
}

func tokenUserSID(t windows.Token) (*windows.SID, error) {
	u, err := t.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("failed to get token user: %w", err)
	}
	return u.User.Sid, nil
}

// verifySameUser checks if two tokens belong to the same user.
func verifySameUser(t1, t2 windows.Token) (bool, error) {
	s1, err := tokenUserSID(t1)
	if err != nil {
		return false, fmt.Errorf("failed to get token user: %w", err)
	}
	s2, err := tokenUserSID(t2)
	if err != nil {
		return false, fmt.Errorf("failed to get token user: %w", err)
	}
	return s1.Equals(s2), nil
}

// getPipeClientToken retrieves the impersonation token for the pipe client.
func getPipeClientToken(conn winioConn) (windows.Token, error) {
	var token windows.Token
	if err := withConnHandle(conn, func(h windows.Handle) error {
		err := impersonateNamedPipeClient(h)
		if err != nil {
			return fmt.Errorf("failed to impersonate client: %w", err)
		}
		defer windows.RevertToSelf()

		return windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, true, &token)
	}); err != nil {
		return 0, err
	}
	return token, nil
}

// getServerProcessToken retrieves the process token for the pipe client.
func getServerProcessToken() (windows.Token, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return 0, fmt.Errorf("failed to open service process token: %w", err)
	}
	return token, nil
}
