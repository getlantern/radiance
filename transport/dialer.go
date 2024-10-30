package transport

import (
	"net/url"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/configurl"

	"github.com/getlantern/radiance/transport/logger"
)

func DialerFrom(proxyConfig string) (transport.StreamDialer, error) {
	dialer := configurl.NewDefaultConfigToDialer()
	dialer.RegisterStreamDialerType("logger",
		func(innerSD func() (transport.StreamDialer, error), _ func() (transport.PacketDialer, error), _ *url.URL) (transport.StreamDialer, error) {
			inner, err := innerSD()
			if err != nil {
				return nil, err
			}
			return logger.NewStreamDialer(inner)
		},
	)

	return dialer.NewStreamDialer(proxyConfig)
}
