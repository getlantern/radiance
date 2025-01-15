package vpn

import (
	"fmt"
	"os/exec"
)

// Cmds for configuring VPN on macOS
// -------------------------------------------------------------------
// # Create a new VPN service														|
// networksetup -createnetworkservice "MyVPN" VPN <vpnType>				|
//																							|
// # Set VPN server address														| <--not sure if any of this
// networksetup -setvpnserver "MyVPN" vpn.example.com						|		is needed
//																							|
// # Connect to VPN service														|
// networksetup -connectvpnservice "MyVPN"									|
// -------------------------------------------------------------------
//
// # Add routing rule (e.g., route all traffic through the VPN)
// route add -net 0.0.0.0/1 tun0

// startRouting sets up a VPN connection, creates the VPN service, and adds the necessary routing rules.
func startRouting(ifceName, svrAddr, gateway string) error {
	err := exec.Command("route", "add", "-net", gateway, ifceName).Run()
	if err != nil {
		return fmt.Errorf("failed to add routing rule: %v", err)
	}
	log.Debugf("added routing rule for target network %s", gateway)
	return nil
}

// stopRouting removes the routing rule and deletes the VPN service.
func stopRouting(ifceName string, gateway string) error {
	err := exec.Command("route", "delete", "-net", gateway, ifceName).Run()
	if err != nil {
		return fmt.Errorf("failed to remove routing rule: %v", err)
	}
	log.Debugf("removed routing rule for target network %s", gateway)
	return nil
}
