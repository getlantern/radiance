/*
Must have an OpenVPN client installed on the system to use this on Windows.
*/
package vpn

import (
	"fmt"

	"github.com/songgao/water"
)

// openTun creates a new TUN device to interact with an existing virtual adapter created by an
// OpenVPN client. Note that the interface must be closed and reopened if any changes are made to
// the IP by DHCP, the user, etc. as they will not be seen by the TUN device.
func openTun(ip, gateway string) (*water.Interface, error) {
	ifce, err := water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Network: ip + "/24",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create TUN Interface: %w", err)
	}

	return ifce, nil
}
