// Package client provides an API for external applications to use and manage radiance VPN.
package client

import "context"

// VPNStatus is a type used for representing possible VPN statuses
type VPNStatus string

const (
	ConnectedVPNStatus    VPNStatus = "connected"
	DisconnectedVPNStatus VPNStatus = "disconnected"
	ConnectingVPNStatus   VPNStatus = "connecting"
)

// APIManager set the minimal functionalities an client must provide for using radiance
type APIManager interface {
	// StartVPN selects a proxy internally and start the VPN.
	StartVPN() error
	// StopVPN stops the VPN and closes the TUN device.
	StopVPN() error
	// VPNStatus checks the current VPN status
	VPNStatus() VPNStatus
	// ActiveProxyLocation returns the proxy server's location if the VPN is connected.
	// If the VPN is disconnected, it returns nil.
	ActiveProxyLocation(ctx context.Context) (*string, error)
	// SetSystemProxy configures the system proxy to route traffic through a specific proxy.
	SetSystemProxy(serverAddr string, port int) error
	// ClearSystemProxy reset the system proxy settings to their default (no proxy).
	ClearSystemProxy() error
}
