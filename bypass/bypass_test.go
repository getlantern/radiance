package bypass

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newConnectProxy returns an httptest.Server that handles HTTP CONNECT
// requests by hijacking the connection and tunneling data to the target.
func newConnectProxy(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT supported", http.StatusMethodNotAllowed)
			return
		}

		target, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer target.Close()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}

		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer clientConn.Close()

		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

		done := make(chan struct{}, 2)
		cp := func(dst, src net.Conn) {
			io.Copy(dst, src)
			done <- struct{}{}
		}
		go cp(target, clientConn)
		go cp(clientConn, target)
		<-done
		<-done
	}))
}

// newEchoServer starts a TCP listener that accepts one connection,
// reads data, and echoes it back. The listener is cleaned up via t.Cleanup.
func newEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
	}()

	return ln.Addr().String()
}

func TestDialContext_ProxyAvailable(t *testing.T) {
	proxy := newConnectProxy(t)
	defer proxy.Close()

	echoAddr := newEchoServer(t)

	proxyConn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	require.NoError(t, err)
	defer proxyConn.Close()

	tunnelConn, err := httpConnect(context.Background(), proxyConn, echoAddr)
	require.NoError(t, err)

	msg := []byte("hello bypass")
	_, err = tunnelConn.Write(msg)
	require.NoError(t, err)

	buf := make([]byte, 64)
	n, err := tunnelConn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, string(msg), string(buf[:n]))
}

func TestDialContext_FallbackWhenProxyDown(t *testing.T) {
	echoAddr := newEchoServer(t)

	conn, err := DialContext(context.Background(), "tcp", echoAddr)
	require.NoError(t, err, "DialContext should fall back to direct dial")
	defer conn.Close()

	msg := []byte("direct fallback")
	_, err = conn.Write(msg)
	require.NoError(t, err)

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, string(msg), string(buf[:n]))
}

func TestDialContext_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := DialContext(ctx, "tcp", "127.0.0.1:1")
	require.Error(t, err, "expected error with cancelled context")
}

func TestHttpConnect_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	_, err = httpConnect(context.Background(), conn, "example.com:443")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}
