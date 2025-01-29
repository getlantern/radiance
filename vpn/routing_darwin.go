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

// startRouting adds the necessary routing rules.
func startRouting(rConf *RoutingConfig, proxyAddr string, bypassUDP bool) error {
	name, gwCIDR := rConf.TunName, rConf.Gw
	log.Debugf("configuring routing for interface %s with gateway %s", name, gwCIDR)
	err := exec.Command("route", "add", "-net", gwCIDR, name).Run()
	if err != nil {
		return fmt.Errorf("failed to add routing rule: %v", err)
	}
	log.Debugf("added routing rule for target network %s", gwCIDR)
	return nil
}

// stopRouting removes the routing rules.
func stopRouting(rConf *RoutingConfig) error {
	log.Debug("removing routing rules")
	err := exec.Command("route", "delete", "-net", rConf.Gw, rConf.TunName).Run()
	if err != nil {
		return fmt.Errorf("failed to remove routing rule: %v", err)
	}
	log.Debugf("removed routing rule for target network %s", rConf.Gw)
	return nil
}
