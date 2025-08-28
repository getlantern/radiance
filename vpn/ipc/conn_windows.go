package ipc

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	pipePath = `\\.\pipe\Lantern\radiance`

	apiURL         = "http://pipe"
	connectTimeout = 10 * time.Second
)

var sddl = `O:SYG:SYD:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;${VPN_USER_SID})(ML;;NW;;;ME)`

func dialContext(_ context.Context, _, _ string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	return winio.DialPipeAccessImpLevel(ctx, pipePath, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}

// listen creates a named pipe listener at a predefined path.
func listen(_ string) (net.Listener, error) {
	return winio.ListenPipe(
		pipePath,
		&winio.PipeConfig{
			SecurityDescriptor: sddl,
			InputBufferSize:    256 * 1024,
			OutputBufferSize:   256 * 1024,
		},
	)
}
