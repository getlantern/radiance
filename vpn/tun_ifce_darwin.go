package vpn

import (
	"fmt"
	"os/exec"

	"github.com/songgao/water"
)

// openTun creates a new TUN device with the given IP address and gateway.
func openTun(ip, gateway string) (*water.Interface, error) {
	ifce, err := water.New(water.Config{
		DeviceType: water.TUN,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create TUN Interface: %w", err)
	}

	// assign the IP and gateway to the TUN interface and bring it up
	if err := exec.Command("ifconfig", ifce.Name(), ip, gateway, "up").Run(); err != nil {
		ifce.Close()
		return nil, fmt.Errorf("failed to set IP address on TUN interface %s: %w", ifce.Name(), err)
	}

	return ifce, nil
}
