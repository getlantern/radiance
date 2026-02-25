package bypass

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// startMockProxy starts a local TCP listener that accepts HTTP CONNECT
// requests and tunnels data to the target. Returns the listener address
// and a cleanup function.
func startMockProxy(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock proxy: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleMockConnect(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func handleMockConnect(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		fmt.Fprintf(conn, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}

	// Connect to the target
	target, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
	if err != nil {
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer target.Close()

	fmt.Fprintf(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	// Bidirectional copy
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 4096)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				dst.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go cp(target, conn)
	go cp(conn, target)
	<-done
}

func TestDialContext_ProxyAvailable(t *testing.T) {
	// Start a mock proxy and a mock target server.
	proxyAddr, cleanup := startMockProxy(t)
	defer cleanup()

	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start target: %v", err)
	}
	defer targetLn.Close()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
	}()

	// Override ProxyAddr for the test by dialing the mock proxy directly.
	// We can't override the const, so test httpConnect directly.
	proxyConn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("failed to connect to mock proxy: %v", err)
	}
	defer proxyConn.Close()

	ctx := context.Background()
	if err := httpConnect(ctx, proxyConn, targetLn.Addr().String()); err != nil {
		t.Fatalf("httpConnect failed: %v", err)
	}

	// Send data through the tunnel and verify echo.
	msg := []byte("hello bypass")
	if _, err := proxyConn.Write(msg); err != nil {
		t.Fatalf("failed to write through tunnel: %v", err)
	}
	buf := make([]byte, 64)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read through tunnel: %v", err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, buf[:n])
	}
}

func TestDialContext_FallbackWhenProxyDown(t *testing.T) {
	// Start a target server that echoes data.
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start target: %v", err)
	}
	defer targetLn.Close()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
	}()

	// DialContext with no proxy running should fall back to direct dial.
	ctx := context.Background()
	conn, err := DialContext(ctx, "tcp", targetLn.Addr().String())
	if err != nil {
		t.Fatalf("DialContext should fall back to direct dial, got error: %v", err)
	}
	defer conn.Close()

	msg := []byte("direct fallback")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, buf[:n])
	}
}

func TestDialContext_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// With a cancelled context, both proxy and direct dial should fail.
	_, err := DialContext(ctx, "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

func TestHttpConnect_BadStatus(t *testing.T) {
	// Start a server that returns 403 for CONNECT.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		http.ReadRequest(br)
		fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\n\r\n")
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	err = httpConnect(context.Background(), conn, "example.com:443")
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
}
