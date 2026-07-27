// Package testsocks provides a SOCKS5 CONNECT proxy standing in for radiance's
// local bypass proxy in tests.
//
// Production runs a sing-box mixed inbound, and the bypass package's own tests
// use one. Callers that only need something on the far end of the proxy port
// take this instead, to keep the whole sing-box stack out of their test binary
// — it has to be pushed to a device on every android run.
package testsocks

import (
	"bufio"
	"io"
	"net"
	"testing"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/protocol/socks/socks5"
	"github.com/stretchr/testify/require"
)

type Server struct {
	ln net.Listener
}

// Listen starts a SOCKS5 CONNECT proxy on addr and closes it on test cleanup.
// Pass port 0 for an ephemeral port and read the resolved one from Addr.
func Listen(t *testing.T, addr string) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	s := &Server{ln: ln}
	go s.serve()
	return s
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	if _, err := socks5.ReadAuthRequest(reader); err != nil {
		return
	}
	if err := socks5.WriteAuthResponse(conn, socks5.AuthResponse{Method: socks5.AuthTypeNotRequired}); err != nil {
		return
	}
	request, err := socks5.ReadRequest(reader)
	if err != nil {
		return
	}
	if request.Command != socks5.CommandConnect {
		socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeUnsupported})
		return
	}

	upstream, err := net.Dial("tcp", request.Destination.String())
	if err != nil {
		socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeForError(err)})
		return
	}
	defer upstream.Close()

	err = socks5.WriteResponse(conn, socks5.Response{
		ReplyCode: socks5.ReplyCodeSuccess,
		Bind:      M.SocksaddrFromNet(upstream.LocalAddr()),
	})
	if err != nil {
		return
	}

	done := make(chan struct{}, 2)
	// Copy from reader, not conn: it may hold bytes the client pipelined behind
	// the CONNECT request.
	go splice(upstream, reader, done)
	go splice(conn, upstream, done)
	<-done
	<-done
}

// splice copies src into dst, then half-closes dst when it supports it, so that
// an upstream waiting on EOF — an echo server, or an HTTP server reading a
// request body — sees the stream end rather than stalling until the test times
// out.
func splice(dst io.Writer, src io.Reader, done chan<- struct{}) {
	io.Copy(dst, src)
	if hc, ok := dst.(interface{ CloseWrite() error }); ok {
		hc.CloseWrite()
	}
	done <- struct{}{}
}
