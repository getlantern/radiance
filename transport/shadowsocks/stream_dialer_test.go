package shadowsocks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/config"
)

func TestStreamDialer(t *testing.T) {
	done := make(chan error, 1)
	listener, err := startServer(done)
	require.NoError(t, err)
	defer listener.Close()

	conf, err := newTestConfig(listener.Addr().String())
	require.NoError(t, err)

	dialer, err := NewStreamDialer(&transport.TCPDialer{}, conf)
	conn, err := dialer.DialStream(context.Background(), listener.Addr().String())
	require.NoError(t, err)

	msg := "Why are you copying me?!"
	n, err := conn.Write([]byte(msg))
	require.NoError(t, err)
	require.Equalf(t, len(msg), n, "wrote %d/%d bytes", n, len(msg))

	// make sure the buffer is large enough to also hold the generated upstream address (upto 22 bytes)
	read := make([]byte, len(msg)*2+22)
	n, err = conn.Read(read)
	if !errors.Is(err, io.EOF) {
		require.NoError(t, err)
	}
	require.Greaterf(t, n, len(msg), "read %d <%d bytes", n, len(msg))
	require.Equal(t, msg, string(read[n-len(msg):n]))

	conn.Close()
	<-done
}

// startServer starts a server that echos back any data sent to it.
func startServer(done chan error) (net.Listener, error) {
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, fmt.Errorf("Could not listen: %w", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				done <- fmt.Errorf("Error accepting connection: %w", err)
				return
			}

			// Echo back
			io.Copy(conn, conn)
			conn.Close()
			done <- nil
		}
	}()

	return listener, nil
}

func newTestConfig(addr string) (*config.Config, error) {
	addr, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	p, _ := strconv.Atoi(port)
	return &config.Config{
		Addr:     addr,
		Port:     int32(p),
		Protocol: "shadowsocks",
		ProtocolConfig: &config.ProxyConnectConfig_ConnectCfgShadowsocks{
			ConnectCfgShadowsocks: &config.ProxyConnectConfig_ShadowsocksConfig{
				Cipher: "aes-256-gcm",
				Secret: "test",
			},
		},
	}, nil
}
