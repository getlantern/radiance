package radiance

import (
	"context"
)

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
	// Run starts the Radiance proxy server on the specified address.
	Run(addr string) error
	// Shutdown stops the Radiance server.
	Shutdown(ctx context.Context) error
	// VPNStatus checks the current VPN status
	VPNStatus() VPNStatus
	// ActiveProxyLocation returns the proxy server's location if the VPN is connected.
	// If the VPN is disconnected, it returns nil.
	ActiveProxyLocation(ctx context.Context) (*string, error)
	// ProxyStatus provides information about the current proxy status like the proxy's
	// location or whether the proxy is connected or not.
	ProxyStatus() <-chan ProxyStatus
}
