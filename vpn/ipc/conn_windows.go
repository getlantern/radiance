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

// winioConn is an helper interface to access the underlying file descriptor of a winio.Conn. This
// is needed to call Windows API functions that require a handle.
type winioConn interface {
	net.Conn
	FD() uintptr
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
	pipeToken, err := getPipeClientToken(wc)
	if err != nil {
		return nil, fmt.Errorf("failed to get pipe client token: %w", err)
	}
	defer pipeToken.Close()

	procToken, err := getProcessToken(wc)
	if err != nil {
		return nil, fmt.Errorf("failed to get process token: %w", err)
	}
	defer procToken.Close()

	// if the tokens are not the same user, reject the connection.
	sameUser, err := verifySameUser(pipeToken, procToken)
	if err != nil {
		return nil, fmt.Errorf("failed to verify same user: %w", err)
	}
	if !sameUser {
		return nil, fmt.Errorf("pipe client and process are not the same user")
	}
	return wc, nil
}

// verifySameUser checks if two tokens belong to the same user.
func verifySameUser(t1, t2 windows.Token) (bool, error) {
	u1, err := t1.GetTokenUser()
	if err != nil {
		return false, fmt.Errorf("failed to get token user: %w", err)
	}
	u2, err := t2.GetTokenUser()
	if err != nil {
		return false, fmt.Errorf("failed to get token user: %w", err)
	}
	return u1.User.Sid.Equals(u2.User.Sid), nil
}

// getPipeClientToken retrieves the impersonation token for the pipe client.
func getPipeClientToken(conn winioConn) (windows.Token, error) {
	ph := windows.Handle(conn.FD())
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
		return 0, fmt.Errorf("failed to open process token: %w", err)
	}
	defer windows.CloseHandle(h)

	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return 0, fmt.Errorf("failed to open process token: %w", err)
	}
	return token, nil
}

func getPipeClientPID(pc winioConn) (uint32, error) {
	ph := windows.Handle(pc.FD())
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
