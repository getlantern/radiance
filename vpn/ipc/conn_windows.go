package ipc

//go:generate go run golang.org/x/sys/windows/mkwinsyscall -output zsyscall_windows.go conn_windows.go

//sys impersonateNamedPipeClient(h windows.Handle) (err error) [int32(failretval)==0] = advapi32.ImpersonateNamedPipeClient

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	pipePath = `\\.\pipe\Lantern\lantern`
	// pipePath = `\\.\pipe\ProtectedPrefix\Administrators\`

	apiURL         = "http://pipe"
	connectTimeout = 10 * time.Second

	//     `D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;S-1-5-21-...)`
	//     `D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;AU)`,
	sddl = `D:P(A;;GA;;;SY)(A;;GRGW;;;BA)(A;;GRGW;;;IU)`
)

func dialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	return winio.DialPipeAccessImpLevel(ctx, pipePath, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}

// listen creates a named pipe listener at a predefined path.
func listen() (net.Listener, error) {
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

// winioConn is a helper interface to access the underlying file descriptor of a winio.Conn. This
// is needed to call Windows API functions that require a handle.
type winioConn interface {
	net.Conn
	Fd() uintptr
}

type winioListener struct {
	net.Listener
}

type winconn struct {
	winioConn
	token windows.Token
}

func (c *winconn) Close() error {
	c.token.Close()
	return c.winioConn.Close()
}

// Accept waits for and returns the next connection to the listener, verifying the client identity.
func (l *winioListener) Accept() (conn net.Conn, err error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()

	// verify that the pipe client is the same user as the process that created the pipe.
	wc, ok := c.(winioConn)
	if !ok {
		return nil, fmt.Errorf("expected winio.Conn, got %T", c)
	}
	token, err := getPipeClientToken(wc)
	if err != nil {
		return nil, fmt.Errorf("failed to get pipe client token: %w", err)
	}
	return &winconn{
		winioConn: wc,
		token:     token,
	}, nil
}

// // withConnHandle runs the function with the connection’s handle pinned by the runtime
// func withConnHandle(c winioConn, fn func(h windows.Handle) error) error {
// 	rc, err := c.SyscallConn()
// 	if err != nil {
// 		return err
// 	}
// 	var callErr error
// 	if err := rc.Control(func(fd uintptr) { callErr = fn(windows.Handle(fd)) }); err != nil {
// 		return err
// 	}
// 	return callErr
// }
//
// // getPipeClientToken retrieves the impersonation token for the pipe client.
// func getPipeClientToken(conn winioConn) (windows.Token, error) {
// 	var token windows.Token
// 	if err := withConnHandle(conn, func(h windows.Handle) error {
// 		err := impersonateNamedPipeClient(h)
// 		if err != nil {
// 			return fmt.Errorf("failed to impersonate client: %w", err)
// 		}
// 		defer windows.RevertToSelf()
//
// 		return windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, true, &token)
// 	}); err != nil {
// 		return 0, err
// 	}
// 	return token, nil
// }

// getPipeClientToken retrieves the impersonation token for the pipe client.
func getPipeClientToken(conn winioConn) (windows.Token, error) {
	ph := windows.Handle(conn.Fd())
	if ph == 0 {
		return 0, fmt.Errorf("invalid pipe handle")
	}

	err := impersonateNamedPipeClient(ph)
	if err != nil {
		return 0, fmt.Errorf("failed to impersonate client: %w", err)
	}
	defer windows.RevertToSelf()

	var token windows.Token
	err = windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, true, &token)
	if err != nil {
		return 0, fmt.Errorf("failed to open thread token: %w", err)
	}
	return token, nil
}

// getProcessToken retrieves the process token for the pipe client.
func getProcessToken(pc winioConn) (windows.Token, error) {
	pid, err := getPipeClientPID(pc)
	if err != nil {
		return 0, fmt.Errorf("failed to get client process id: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return 0, fmt.Errorf("failed to open process handle: %w", err)
	}
	defer windows.CloseHandle(h)

	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return 0, fmt.Errorf("failed to open process token: %w", err)
	}
	return token, nil
}

func getPipeClientPID(pc winioConn) (uint32, error) {
	ph := windows.Handle(pc.Fd())
	if ph == 0 {
		return 0, fmt.Errorf("invalid pipe handle")
	}
	var pid uint32
	err := windows.GetNamedPipeClientProcessId(ph, &pid)
	if err != nil {
		return 0, fmt.Errorf("failed to get client process id: %w", err)
	}
	return pid, nil
}

func getConnPeer(conn net.Conn) (p usr, err error) {
	wc, ok := conn.(*winconn)
	if !ok {
		return p, fmt.Errorf("expected *winconn, got %T", conn)
	}
	return usrFromToken(wc.token)
}
