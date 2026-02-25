package bypass

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
)

const (
	// ProxyAddr is the address of the local bypass proxy listener in the VPN process.
	ProxyAddr = "127.0.0.1:14985"

	// BypassInboundTag is the sing-box inbound tag used for routing bypass traffic to direct.
	BypassInboundTag = "bypass-in"
)

// DialContext tries to connect through the local bypass proxy. If the proxy is
// not reachable (VPN not running), it falls back to a direct dial.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", ProxyAddr)
	if err != nil {
		// Proxy not running → VPN not active → dial directly
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	if err := httpConnect(proxyConn, addr); err != nil {
		proxyConn.Close()
		return nil, err
	}
	return proxyConn, nil
}

// Dial is a convenience wrapper without context, suitable for use with
// amp.WithDialer which expects func(network, addr string) (net.Conn, error).
func Dial(network, addr string) (net.Conn, error) {
	return DialContext(context.Background(), network, addr)
}

// httpConnect performs an HTTP CONNECT handshake over an already-established
// connection to the proxy.
func httpConnect(conn net.Conn, addr string) error {
	req := &http.Request{
		Method: http.MethodConnect,
		Host:   addr,
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("bypass: failed to write CONNECT request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("bypass: failed to read CONNECT response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bypass: CONNECT returned status %d", resp.StatusCode)
	}
	return nil
}
