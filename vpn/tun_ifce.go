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
	ifce, err := openTun(ip)
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
