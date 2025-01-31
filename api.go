package radiance

import (
	"context"
)

// APIManager set the minimal functionalities an client must provide for using radiance
type APIManager interface {
	// Run starts the Radiance proxy server on the specified address.
	Run(addr string) error
	// Shutdown stops the Radiance server.
	Shutdown(ctx context.Context) error
	// TUNStatus returns the current status of the TUN device and routing configuration,
	// indicating whether the local VPN is active, disconnected, or in the process of connecting.
	TUNStatus() TUNStatus
	// ActiveProxyLocation returns the proxy server's location if the VPN is connected.
	// If the VPN is disconnected, it returns nil.
	ActiveProxyLocation(ctx context.Context) (*string, error)
	// ProxyStatus provides information about the current proxy status like the proxy's
	// location or whether the proxy is connected or not.
	ProxyStatus() <-chan ProxyStatus
}
