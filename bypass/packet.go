package bypass

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

// generation counts bypass-proxy starts and stops, so long-lived packet conns
// know when to re-pick their route.
var generation atomic.Uint64

// ProxyStateChanged tells packet conns from ListenPacket that the bypass proxy
// came up or went down. The tunnel calls it after the proxy starts and after it
// stops; the conns switch route on their next write.
func ProxyStateChanged() { generation.Add(1) }

const reopenBackoff = time.Second

// ListenPacket returns a UDP PacketConn that stays outside the VPN tunnel. While
// the bypass proxy runs it relays through a SOCKS5 UDP association; otherwise it
// is a plain socket. Unlike a dialed stream, it outlives VPN starts and stops,
// re-picking its route whenever the proxy state changes or its association dies.
func ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	c := &packetConn{network: network, address: address}
	if err := c.reopen(ctx, nil); err != nil {
		return nil, err
	}
	return c, nil
}

type packetConn struct {
	network, address string

	mu     sync.Mutex
	cur    net.PacketConn
	gen    uint64
	closed bool
	// Deadlines carry over to replacement conns.
	readDeadline, writeDeadline time.Time
}

func (c *packetConn) current() (net.PacketConn, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur, c.gen, c.closed
}

// reopen replaces old (nil for the first open) with a conn routed for the
// proxy's current state, unless another goroutine already replaced it.
func (c *packetConn) reopen(ctx context.Context, old net.PacketConn) error {
	gen := generation.Load()
	pc, err := openPacket(ctx, c.network, c.address)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed || c.cur != old {
		c.mu.Unlock()
		pc.Close()
		if c.closed {
			return net.ErrClosed
		}
		return nil
	}
	pc.SetReadDeadline(c.readDeadline)
	pc.SetWriteDeadline(c.writeDeadline)
	c.cur, c.gen = pc, gen
	c.mu.Unlock()
	if old != nil {
		old.Close() // unblocks ReadFrom, which moves to the new conn
	}
	return nil
}

func (c *packetConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	pc, gen, closed := c.current()
	if closed {
		return 0, net.ErrClosed
	}
	if gen != generation.Load() {
		if err := c.reopen(context.Background(), pc); err != nil {
			return 0, err
		}
		if pc, _, _ = c.current(); pc == nil {
			return 0, net.ErrClosed
		}
	}
	return pc.WriteTo(p, addr)
}

func (c *packetConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		pc, _, closed := c.current()
		if closed {
			return 0, nil, net.ErrClosed
		}
		n, addr, err := pc.ReadFrom(p)
		if err == nil {
			return n, addr, nil
		}
		if next, _, closed := c.current(); closed {
			return 0, nil, net.ErrClosed
		} else if next != pc {
			continue // swapped under us
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return 0, nil, err
		}
		// The association died (proxy stopped or idled it out) or the socket
		// failed: route afresh, backing off so a dead network doesn't spin.
		if c.reopen(context.Background(), pc) != nil {
			time.Sleep(reopenBackoff)
		}
	}
}

func (c *packetConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.cur.Close()
}

func (c *packetConn) LocalAddr() net.Addr {
	pc, _, _ := c.current()
	return pc.LocalAddr()
}

func (c *packetConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline, c.writeDeadline = t, t
	return c.cur.SetDeadline(t)
}

func (c *packetConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	return c.cur.SetReadDeadline(t)
}

func (c *packetConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return c.cur.SetWriteDeadline(t)
}

// openPacket associates through the bypass proxy, or, when the proxy isn't
// listening (VPN not running), opens a plain socket.
func openPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	pc, err := associate(ctx)
	if err == nil {
		return pc, nil
	}
	var unreachable *proxyUnreachable
	if !errors.As(err, &unreachable) {
		return nil, err
	}
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, network, address)
}

// associate opens a SOCKS5 UDP association. Its lifetime is the control TCP
// conn's, so a watcher closes the UDP side when the proxy hangs up.
func associate(ctx context.Context) (net.PacketConn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	server := M.ParseSocksaddrHostPort("127.0.0.1", uint16(ProxyPort))
	tcp, err := (&net.Dialer{}).DialContext(ctx, "tcp", server.String())
	if err != nil {
		return nil, &proxyUnreachable{err}
	}
	if deadline, ok := ctx.Deadline(); ok {
		tcp.SetDeadline(deadline)
	}
	resp, err := socks.ClientHandshake5(tcp, socks5.CommandUDPAssociate, M.Socksaddr{}, "", "")
	if err != nil {
		tcp.Close()
		return nil, err
	}
	tcp.SetDeadline(time.Time{})
	bind := resp.Bind
	if !bind.Addr.IsValid() || bind.Addr.IsUnspecified() {
		bind.Addr = server.Addr
	}
	udp, err := (&net.Dialer{}).DialContext(ctx, "udp", bind.String())
	if err != nil {
		tcp.Close()
		return nil, err
	}
	go func() {
		io.Copy(io.Discard, tcp)
		udp.Close()
	}()
	return socks.NewAssociatePacketConn(udp, M.Socksaddr{}, tcp), nil
}
