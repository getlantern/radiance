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

// ProxyStatus provide
type ProxyStatus struct {
	Connected bool
	// Location provides the proxy's geographical location. If connected is false,
	// the value will be a empty string.
	Location string
}

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
	// ProxyStatus provides information about the current proxy status like the proxy's
	// location or whether the proxy is connected or not.
	ProxyStatus() <-chan ProxyStatus
	// SetSystemProxy configures the system proxy to route traffic through a specific proxy.
	SetSystemProxy(serverAddr string, port int) error
	// ClearSystemProxy reset the system proxy settings to their default (no proxy).
	ClearSystemProxy() error
}
