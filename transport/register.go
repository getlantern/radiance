package transport

import (
	"github.com/getlantern/radiance/transport/algeneva"
	"github.com/getlantern/radiance/transport/logger"
	"github.com/getlantern/radiance/transport/multiplex"
	"github.com/getlantern/radiance/transport/proxyless"
	"github.com/getlantern/radiance/transport/shadowsocks"
)

// init registers the dialer builders for the supported protocols.
func init() {
	registerDialerBuilder("logger", logger.NewStreamDialer)
	registerDialerBuilder("multiplex", multiplex.NewStreamDialer)
	registerDialerBuilder("shadowsocks", shadowsocks.NewStreamDialer)
	registerDialerBuilder("algeneva", algeneva.NewStreamDialer)
	registerDialerBuilder("proxyless", proxyless.NewStreamDialer)
}
