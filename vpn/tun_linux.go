package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
)

// openTun creates a new TUN device with the given IP address.
func openTunDevice(rConf *RoutingConfig) (_ io.ReadWriteCloser, err error) {
	var ifce *water.Interface
	ifce, err = water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name: rConf.TunName,
			// do not persist the TUN device after the process exits
			Persist: false,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create TUN Interface: %w", err)
	}

	defer func() {
		if err != nil {
			ifce.Close()
		}
	}()

	// wait for the TUN interface to be created
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link, err := waitForTun(ctx, ifce.Name())
	if err != nil {
		return nil, fmt.Errorf("could not find TUN interface %s: %w", ifce.Name(), err)
	}
	if err = addSubnet(link, rConf.TunIP); err != nil {
		return nil, fmt.Errorf("failed to add subnet: %w", err)
	}
	log.Debugf("bringing up TUN interface %s", ifce.Name())
	if err := bringUpTun(link); err != nil {
		return nil, fmt.Errorf("failed to bring up TUN interface: %w", err)
	}

	return ifce, nil
}

// addSubnet adds the subnet to the TUN interface.
func addSubnet(link netlink.Link, ip string) error {
	subnet, err := netlink.ParseAddr(ip + "/32")
	if err != nil {
		return fmt.Errorf("subnet IP address %s invalid: %w", ip, err)
	}
	log.Debugf("adding subnet %s", subnet)
	if err := netlink.AddrAdd(link, subnet); err != nil {
		return fmt.Errorf("failed to add subnet: %w", err)
	}
	return nil
}

// bringUpTun assigns the specified IP address to the TUN interface and brings it up.
func bringUpTun(link netlink.Link) error {
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up TUN interface: %w", err)
	}
	return nil
}

// closeTun closes the TUN interface.
func closeTun(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("could not find TUN interface %s: %w", name, err)
	}
	log.Debugf("bringing down TUN interface %s", name)
	if err = netlink.LinkSetDown(link); err != nil {
		return fmt.Errorf("failed to bring down TUN interface %s: %w", name, err)
	}
	log.Debugf("deleting TUN interface %s", name)
	return netlink.LinkDel(link)
}

func waitForTun(ctx context.Context, name string) (netlink.Link, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("timeout waiting for TUN interface")
		case <-time.After(50 * time.Millisecond):
			link, _ := netlink.LinkByName(name)
			if link != nil {
				return link, nil
			}
		}
	}
}
