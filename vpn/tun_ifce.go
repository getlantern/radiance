package vpn

import (
	"fmt"
	"net"

	"github.com/Jigsaw-Code/outline-sdk/network"
	"github.com/songgao/water"
)

// tunIfce is a wrapper around a TUN network interface.
type tunIfce struct {
	*water.Interface
	ifce *net.Interface
}

// openTunIfce opens a TUN network interface and assigns it the specified IP address.
func openTunIfce(ip string) (tun network.IPDevice, err error) {
	addr := net.ParseIP(ip)
	if addr == nil {
		return nil, fmt.Errorf("invalid IP address: %s", ip)
	}
	gw := addr.To4()
	gw[3]++ // Increment the last octet to get the gateway address
	ifce, err := openTun(ip, gw.String())
	if err != nil {
		return nil, err
	}

	name := ifce.Name()
	nIfce, err := net.InterfaceByName(name)
	if err != nil {
		ifce.Close()
		return nil, fmt.Errorf("could not find TUN interface %s: %w", name, err)
	}

	return &tunIfce{Interface: ifce, ifce: nIfce}, nil
}

// MTU implements the [network.IPDevice] interface.
func (t *tunIfce) MTU() int {
	return t.ifce.MTU
}

func (t *tunIfce) Close() error {
	if err := t.Interface.Close(); err != nil {
		return fmt.Errorf("failed to close TUN interface %s: %w", t.Name(), err)
	}
	return closeTun(t.Name())
}
