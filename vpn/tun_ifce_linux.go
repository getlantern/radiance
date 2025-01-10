package vpn

import (
	"fmt"

	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
)

// openTun creates a new TUN device with the given IP address and gateway.
func openTun(ip, gateway string) (*water.Interface, error) {
	ifce, err := water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			// do not persist the TUN device after the process exits
			Persist: false,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create TUN Interface: %w", err)
	}

	if err := bringUpTun(ifce.Name(), ip); err != nil {
		ifce.Close()
		return nil, err
	}

	return ifce, nil
}

// bringUpTun assigns the specified IP address to the TUN interface and brings it up.
func bringUpTun(name, ip string) error {
	tunLink, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("could not find TUN interface %s: %w", name, err)
	}
	addr, err := netlink.ParseAddr(ip + "/24")
	if err != nil {
		return fmt.Errorf("subnet IP address %s invalid: %w", ip, err)
	}
	if err = netlink.AddrAdd(tunLink, addr); err != nil {
		return fmt.Errorf("failed to set IP address on TUN interface %s: %w", name, err)
	}
	if err = netlink.LinkSetUp(tunLink); err != nil {
		return fmt.Errorf("failed to bring up TUN interface %s: %w", name, err)
	}
	return nil
}
