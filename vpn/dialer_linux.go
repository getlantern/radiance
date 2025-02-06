package vpn

import (
	"fmt"
	"net"
	"syscall"

	"github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/radiance/config"
	rtransport "github.com/getlantern/radiance/transport"
)

const fwmark = 0x711e

func newDialerListener(proxyConf *config.Config) (transport.StreamDialer, transport.PacketListener, error) {
	base := newFWMarkTCPDialer()
	dialer, err := rtransport.DialerFromWithBase(base, proxyConf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create dialer: %w", err)
	}
	return dialer, newFWMarkUDPListener(), nil
}

// newFWMarkTCPDialer creates a base TCP dialer marked by the specified firewall mark.
func newFWMarkTCPDialer() transport.StreamDialer {
	return &transport.TCPDialer{
		Dialer: net.Dialer{
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, int(fwmark))
				})
			},
		},
	}
}

// newFWMarkUDPDialer creates a new UDP dialer marked by the specified firewall mark.
func newFWMarkUDPDialer() transport.PacketDialer {
	return &transport.UDPDialer{
		Dialer: net.Dialer{
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, int(fwmark))
				})
			},
		},
	}
}

// newFWMarkUDPListener creates a new UDP listener marked by the specified firewall mark.
func newFWMarkUDPListener() transport.PacketListener {
	return &transport.UDPListener{
		ListenConfig: net.ListenConfig{
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, int(fwmark))
				})
			},
		},
	}
}
