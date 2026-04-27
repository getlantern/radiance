package bypass

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/log"
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

	// dialTimeout is the timeout for establishing the initial TCP connection.
	dialTimeout = 30 * time.Second

	// dialKeepAlive is the interval for TCP keep-alive probes.
	dialKeepAlive = 30 * time.Second
)

// DialContext tries to connect through the local bypass proxy. If the proxy is
// not reachable (VPN not running), it falls back to a direct dial.
//
// QA: when env.OutboundSocksAddress is set, both the bypass-proxy path and the
// direct-fallback path are replaced by a dial through that upstream SOCKS5,
// so every dial out of radiance goes via the same residential egress.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d, ok := outboundSocksDialer(); ok {
		return d.DialContext(ctx, network, addr)
	}
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
	proxyConn, err := dialer.DialContext(ctx, "tcp", ProxyAddr)
	if err != nil {
		slog.Log(nil, log.LevelTrace, "bypass proxy not reachable, falling back to direct dial", "addr", addr, "error", err)
		return dialer.DialContext(ctx, network, addr)
	}
	tunnelConn, err := httpConnect(ctx, proxyConn, addr)
	if err != nil {
		proxyConn.Close()
		return nil, err
	}
	return tunnelConn, nil
}

var (
	outboundSocksOnce   sync.Once
	outboundSocksDialFn proxy.ContextDialer
)

// outboundSocksDialer returns a SOCKS5 ContextDialer for env.OutboundSocksAddress
// if set, cached after the first successful build.
func outboundSocksDialer() (proxy.ContextDialer, bool) {
	outboundSocksOnce.Do(func() {
		addr, ok := env.Get(env.OutboundSocksAddress)
		if !ok || addr == "" {
			return
		}
		d, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
		if err != nil {
			slog.Error("invalid RADIANCE_OUTBOUND_SOCKS_ADDRESS for bypass dialer", slog.Any("error", err), slog.String("addr", addr))
			return
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			outboundSocksDialFn = cd
		}
	})
	return outboundSocksDialFn, outboundSocksDialFn != nil
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
