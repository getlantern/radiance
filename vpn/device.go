package vpn

import (
	"fmt"

	"github.com/Jigsaw-Code/outline-sdk/network"
	"github.com/Jigsaw-Code/outline-sdk/network/dnstruncate"
	"github.com/Jigsaw-Code/outline-sdk/network/lwip2transport"
	otransport "github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport"
)

// device is a wrapper around a network.IPDevice that uses a StreamDialer to send and receive packets.
type device struct {
	network.IPDevice
	sd otransport.StreamDialer
	pp network.PacketProxy
}

// newDevice creates a new device using the provided configuration to create a StreamDialer.
func newDevice(conf *config.Config) (*device, error) {
	sd, err := transport.DialerFrom(conf)
	if err != nil {
		return nil, fmt.Errorf("failed to create dialer: %w", err)
	}
	// we use dnstruncate packet proxy until we implement [network.PacketProxy] for transports. This
	// will drop all non-DNS UDP packets.
	pp, err := dnstruncate.NewPacketProxy()
	ipDevice, err := lwip2transport.ConfigureDevice(sd, pp)
	if err != nil {
		return nil, err
	}
	return &device{IPDevice: ipDevice, sd: sd, pp: pp}, nil
}
