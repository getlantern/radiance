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
// route add -net 0.0.0.0/1 -interface <tunName>

// startRouting adds the necessary routing rules.
func startRouting(rConf *RoutingConfig, proxyAddr string, bypassUDP bool) error {
	err := exec.Command("route", "add", "-net", "0.0.0.0/1", "-interface", rConf.TunName).Run()
	if err != nil {
		return fmt.Errorf("failed to add routing rule: %v", err)
	}
	log.Debugf("Added routing table: 0.0.0.0/1 -> %s", rConf.TunName)
	return nil
}

// stopRouting removes the routing rules.
func stopRouting(rConf *RoutingConfig) error {
	// darwin will automatically remove it when the tun device is closed
	return nil
}
