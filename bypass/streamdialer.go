package bypass

import (
	"context"
	"net"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

// StreamDialer returns a transport.StreamDialer that wraps DialContext.
//
// Use this when handing kindling (or any other Outline-SDK-shaped consumer)
// a dialer that should route around the VPN tunnel radiance is serving.
// The smart strategy's connection attempts go through the local bypass
// proxy (and fall back to a direct dial when the proxy isn't listening),
// so kindling traffic doesn't loop back through the tunnel.
func StreamDialer() transport.StreamDialer {
	return transport.FuncStreamDialer(func(ctx context.Context, addr string) (transport.StreamConn, error) {
		conn, err := DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return halfCloseConn{Conn: conn}, nil
	})
}

// halfCloseConn adapts a net.Conn to transport.StreamConn by adding
// CloseRead / CloseWrite. Underlying conns that already implement
// half-close are used as-is; the rest fall back to full Close, which is
// a safe approximation for the smart strategy's probe paths (it does not
// enforce HTTP/1.1 EOF semantics on probe streams).
type halfCloseConn struct {
	net.Conn
}

func (c halfCloseConn) CloseRead() error {
	if hc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return hc.CloseRead()
	}
	return c.Conn.Close()
}

func (c halfCloseConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}
