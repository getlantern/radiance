//go:build darwin

package vpn

import (
	"net"
	"net/netip"
	"testing"

	"github.com/sagernet/sing/common/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultIPv4Interface returns the interface holding the source address of
// an unscoped route to the probe address, if there is one.
func defaultIPv4Interface(t *testing.T) *control.Interface {
	conn, err := net.Dial("udp4", netip.AddrPortFrom(netip.AddrFrom4(sourceProbeAddr), 53).String())
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()
	local := conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	ifaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, ni := range ifaces {
		for _, a := range interfaceIPv4s(&ni) {
			if a == local {
				return &control.Interface{Name: ni.Name, Index: ni.Index}
			}
		}
	}
	return nil
}

func TestCheckSourceAddrDefaultInterface(t *testing.T) {
	iface := defaultIPv4Interface(t)
	if iface == nil {
		res := checkSourceAddr(&control.Interface{Name: "none", Index: 1 << 20})
		assert.Equal(t, sourceUnknown, res.status)
		t.Skip("no default IPv4 interface")
	}
	res := checkSourceAddr(iface)
	t.Logf("iface=%s index=%d addrs=%v source=%s", res.iface, res.index, res.addrs, res.source)
	assert.Equal(t, sourceHealthy, res.status)
	assert.Contains(t, res.addrs, res.source)
	assert.Empty(t, res.bound, "the bound socket is tried only when stale")
}

func TestCheckSourceAddrUnknown(t *testing.T) {
	res := checkSourceAddr(&control.Interface{Name: "gone", Index: 1 << 20})
	assert.Equal(t, sourceUnknown, res.status)
	assert.Equal(t, "iface_gone", res.source)

	// The bound socket on a source the interface holds connects with it.
	if iface := defaultIPv4Interface(t); iface != nil {
		ni, err := net.InterfaceByIndex(iface.Index)
		require.NoError(t, err)
		addr := interfaceIPv4s(ni)[0]
		src, err := connectScoped(iface.Index, addr)
		require.NoError(t, err)
		assert.Equal(t, addr, src)
	}
}
