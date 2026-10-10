//go:build !novpn

package vpn

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	"github.com/miekg/dns"
	sbox "github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/bypass"
)

// fakeResolver echoes each packet back prefixed with its name, and reports the
// source each packet arrived from.
type fakeResolver struct {
	name string
	pc   net.PacketConn
	seen chan seenPacket
}

type seenPacket struct {
	from    *net.UDPAddr
	payload []byte
}

func newFakeResolver(t *testing.T, name string) *fakeResolver {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	r := &fakeResolver{name: name, pc: pc, seen: make(chan seenPacket, 16)}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			r.seen <- seenPacket{from.(*net.UDPAddr), append([]byte(nil), buf[:n]...)}
			pc.WriteTo(append([]byte(name), buf[:n]...), from)
		}
	}()
	return r
}

// startBypassBox runs sing-box with the tunnel build's bypass inbound and real
// routing rules, minus the TUN device, which needs root.
func startBypassBox(t *testing.T) *sbox.Box {
	loopback := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	opts := O.Options{
		Log: &O.LogOptions{Disabled: true},
		DNS: &O.DNSOptions{RawDNSOptions: O.RawDNSOptions{
			Servers: []O.DNSServerOptions{{Tag: "dns_local", Type: C.DNSTypeLocal, Options: &O.LocalDNSServerOptions{}}},
			Final:   "dns_local",
		}},
		Inbounds: []O.Inbound{{
			Type: C.TypeMixed,
			Tag:  bypass.BypassInboundTag,
			Options: &O.HTTPMixedInboundOptions{ListenOptions: O.ListenOptions{
				Listen:     &loopback,
				ListenPort: bypass.ProxyPort,
			}},
		}},
		Outbounds: []O.Outbound{
			{Type: C.TypeDirect, Tag: "direct", Options: &O.DirectOutboundOptions{}},
			{Type: C.TypeBlock, Tag: "block", Options: &O.StubOptions{}},
		},
		Route: &O.RouteOptions{
			Rules:   append(baseRoutingRules(), catchAllBlockerRule()),
			RuleSet: splitTunnelRuleSet(t.TempDir()),
		},
	}
	instance, err := sbox.New(sbox.Options{Context: box.Context(context.Background()), Options: opts})
	require.NoError(t, err)
	require.NoError(t, instance.Start())
	bypass.ProxyStateChanged()
	return instance
}

func stopBypassBox(t *testing.T, b *sbox.Box) {
	require.NoError(t, b.Close())
	bypass.ProxyStateChanged()
}

// exchange sends a TXT query to each resolver and checks it arrives byte for
// byte (not answered by sing-box's DNS hijack) and the reply comes back from
// the right address. It returns whether the query left through the proxy.
func exchange(t *testing.T, pc net.PacketConn, resolvers ...*fakeResolver) (proxied bool) {
	for i, r := range resolvers {
		q := new(dns.Msg)
		q.SetQuestion(fmt.Sprintf("q%d.t.example.com.", i), dns.TypeTXT)
		q.Id = uint16(i + 1)
		wire, err := q.Pack()
		require.NoError(t, err)
		to := r.pc.LocalAddr().(*net.UDPAddr)

		// Retry: the first write after a route switch can race the new association.
		var got seenPacket
		require.Eventually(t, func() bool {
			_, err := pc.WriteTo(wire, to)
			require.NoError(t, err)
			select {
			case got = <-r.seen:
				return true
			case <-time.After(200 * time.Millisecond):
				return false
			}
		}, 5*time.Second, 10*time.Millisecond, "query never reached %s", r.name)
		require.Equal(t, wire, got.payload, "query altered in transit")
		proxied = got.from.Port != pc.LocalAddr().(*net.UDPAddr).Port

		buf := make([]byte, 2048)
		require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
		for {
			n, from, err := pc.ReadFrom(buf)
			require.NoError(t, err)
			if string(buf[:n]) != r.name+string(wire) {
				continue // a duplicate reply from a retry above
			}
			require.Equal(t, to.String(), from.String())
			break
		}
		require.NoError(t, pc.SetReadDeadline(time.Time{}))
	}
	return proxied
}

func TestBypassUDPFollowsProxy(t *testing.T) {
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", bypass.ProxyPort)); err != nil {
		t.Skipf("bypass port busy (VPN running?): %v", err)
	} else {
		l.Close()
	}
	a, b := newFakeResolver(t, "a"), newFakeResolver(t, "b")

	// Opened before the VPN: a plain socket.
	pc, err := bypass.ListenPacket(context.Background(), "udp", ":0")
	require.NoError(t, err)
	defer pc.Close()
	require.False(t, exchange(t, pc, a, b), "expected a direct send with no proxy")

	// VPN up: the same conn moves into a UDP association, and DNS-shaped
	// packets reach the resolvers untouched rather than being hijacked.
	instance := startBypassBox(t)
	require.True(t, exchange(t, pc, a, b), "expected a send through the bypass proxy")

	// VPN down: back to a plain socket.
	stopBypassBox(t, instance)
	require.False(t, exchange(t, pc, a, b), "expected a direct send after the proxy stopped")

	// And up again.
	instance = startBypassBox(t)
	defer stopBypassBox(t, instance)
	require.True(t, exchange(t, pc, a, b), "expected the bypass proxy again")
}
