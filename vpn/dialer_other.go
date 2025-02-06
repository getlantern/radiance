//go:build !linux

package vpn

import (
	"fmt"

	"github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/radiance/config"
	rtransport "github.com/getlantern/radiance/transport"
)

func newDialerListener(proxyConf *config.Config) (transport.StreamDialer, transport.PacketListener, error) {
	dialer, err := rtransport.DialerFrom(proxyConf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create dialer: %w", err)
	}
	return dialer, transport.UDPListener{}, nil
}
