//go:build darwin

package vpn

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"syscall"

	"github.com/sagernet/sing/common/control"
	"golang.org/x/sys/unix"
)

// sourceProbeAddr is where the check's sockets connect. A connected UDP
// socket only selects its route and source; nothing is sent.
var sourceProbeAddr = [4]byte{1, 1, 1, 1}

// checkSource is nil where there is no stale source checker.
var checkSource sourceCheckFunc = checkSourceAddr

// checkSourceAddr opens a socket the way sing-box does, scoped to iface with
// IP_BOUND_IF and no local address, in this process so it shares sing-box's
// network policy, and compares its source with iface's current addresses.
func checkSourceAddr(iface *control.Interface) sourceCheckResult {
	res := sourceCheckResult{iface: iface.Name, index: iface.Index}
	ni, err := net.InterfaceByIndex(iface.Index)
	if err != nil {
		res.source = "iface_gone"
		return res
	}
	addrs := interfaceIPv4s(ni)
	for _, a := range addrs {
		res.addrs = append(res.addrs, lastOctet(a))
	}
	if len(addrs) == 0 {
		res.source = "no_ipv4"
		return res
	}
	src, err := connectScoped(iface.Index, netip.Addr{})
	switch {
	case err == nil:
		res.source = lastOctet(src)
		if slices.Contains(addrs, src) {
			res.status = sourceHealthy
			return res
		}
		res.status = sourceStale
	case errors.Is(err, unix.EADDRNOTAVAIL):
		res.source = errnoClass(err)
		res.status = sourceStale
	default:
		res.source = errnoClass(err)
		return res
	}
	// Would binding to the current address, as sing-box doesn't, get past it?
	bound, err := connectScoped(iface.Index, addrs[0])
	switch {
	case err != nil:
		res.bound = errnoClass(err)
	case bound != addrs[0]:
		res.bound = "source " + lastOctet(bound)
	default:
		res.bound = "ok"
		res.boundOK = true
	}
	return res
}

// connectScoped connects a UDP socket scoped to the interface at index,
// bound to local first if it is valid, and returns its source address.
func connectScoped(index int, local netip.Addr) (netip.Addr, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return netip.Addr{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	unix.CloseOnExec(fd)
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_BOUND_IF, index); err != nil {
		return netip.Addr{}, err
	}
	if local.IsValid() {
		if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: local.As4()}); err != nil {
			return netip.Addr{}, err
		}
	}
	if err := unix.Connect(fd, &unix.SockaddrInet4{Port: 53, Addr: sourceProbeAddr}); err != nil {
		return netip.Addr{}, err
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return netip.Addr{}, err
	}
	sa4, ok := sa.(*unix.SockaddrInet4)
	if !ok {
		return netip.Addr{}, errors.New("not an IPv4 socket address")
	}
	return netip.AddrFrom4(sa4.Addr), nil
}

func interfaceIPv4s(ni *net.Interface) []netip.Addr {
	ifAddrs, err := ni.Addrs()
	if err != nil {
		return nil
	}
	var addrs []netip.Addr
	for _, a := range ifAddrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if addr, ok := netip.AddrFromSlice(ipNet.IP); ok && addr.Unmap().Is4() {
			addrs = append(addrs, addr.Unmap())
		}
	}
	return addrs
}

// errnoClass names err's errno, so a log line carries no address.
func errnoClass(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return "other"
}
