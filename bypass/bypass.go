package bypass

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// ProxyPort is the port for the local bypass proxy listener.
	ProxyPort = 14985

	// ProxyAddr is the address of the local bypass proxy listener in the VPN process.
	ProxyAddr = "127.0.0.1:14985"

	// BypassInboundTag is the sing-box inbound tag used for routing bypass traffic to direct.
	BypassInboundTag = "bypass-in"

	// connectTimeout is the default timeout for the HTTP CONNECT handshake
	// when the caller's context has no deadline.
	connectTimeout = 10 * time.Second
)

// DialContext tries to connect through the local bypass proxy. If the proxy is
// not reachable (VPN not running), it falls back to a direct dial.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", ProxyAddr)
	if err != nil {
		slog.Debug("bypass proxy not reachable, falling back to direct dial", "addr", addr, "error", err)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	tunnelConn, err := httpConnect(ctx, proxyConn, addr)
	if err != nil {
		proxyConn.Close()
		return nil, err
	}
	return tunnelConn, nil
}

// Dial is a convenience wrapper without context, suitable for use with
// amp.WithDialer which expects func(network, addr string) (net.Conn, error).
func Dial(network, addr string) (net.Conn, error) {
	return DialContext(context.Background(), network, addr)
}

// bufferedConn wraps a net.Conn with a bufio.Reader so that any bytes
// buffered during the HTTP CONNECT response read are not lost.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.br.Read(b)
}

// httpConnect performs an HTTP CONNECT handshake over an already-established
// connection to the proxy. It returns a wrapped connection that preserves any
// bytes buffered during the response read. It respects the context deadline;
// if none is set, a default timeout is applied for the handshake and cleared
// afterward.
func httpConnect(ctx context.Context, conn net.Conn, addr string) (net.Conn, error) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(connectTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("bypass: failed to set deadline: %w", err)
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("bypass: failed to write CONNECT request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("bypass: failed to read CONNECT response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bypass: CONNECT returned status %d", resp.StatusCode)
	}

	// Clear deadline so subsequent I/O on the tunneled connection isn't constrained.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("bypass: failed to clear deadline: %w", err)
	}
	return &bufferedConn{Conn: conn, br: br}, nil
}
