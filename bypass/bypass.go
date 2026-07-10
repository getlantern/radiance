package bypass

import (
	"context"
	"net"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
)

const (
	// ProxyPort is the port for the local bypass proxy listener.
	ProxyPort = 14985

	// BypassInboundTag is the sing-box inbound tag used for routing bypass traffic to direct.
	BypassInboundTag = "bypass-in"

	dialTimeout   = 30 * time.Second
	dialKeepAlive = 30 * time.Second
)

// proxyClient dials through the local bypass proxy. We use SOCKS5
// because sing-box's HTTP inbound returns 200 OK for CONNECT before
// the upstream connection is established, falsely signaling success
// to callers.
var proxyClient = socks.NewClient(
	N.SystemDialer,
	M.ParseSocksaddrHostPort("127.0.0.1", uint16(ProxyPort)),
	socks.Version5,
	"", "",
)

// DialContext uses the local SOCKS5 bypass proxy when it is available.
// If the proxy itself is down, it falls back to a direct dial.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := proxyClient.DialContext(ctx, network, M.ParseSocksaddr(addr))
	if err != nil {
		dialer := &net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
		}
		return dialer.DialContext(ctx, network, addr)
	}
	return &tunneledConn{conn}, nil
}

// tunneledConn masks the loopback socket to the bypass proxy so StreamDialer
// keeps it away from strategies like disorder that manipulate the target
// socket directly.
type tunneledConn struct {
	net.Conn
}

// Dial is a convenience wrapper without context, suitable for use with
// amp.WithDialer which expects func(network, addr string) (net.Conn, error).
func Dial(network, addr string) (net.Conn, error) {
	return DialContext(context.Background(), network, addr)
}
