package transport

import (
	"github.com/getlantern/radiance/transport/logger"
	"github.com/getlantern/radiance/transport/multiplex"
	"github.com/getlantern/radiance/transport/shadowsocks"
)

func init() {
	RegisterDialerBuilder("logger", logger.NewStreamDialer)
	RegisterDialerBuilder("multiplex", multiplex.NewStreamDialer)
	RegisterDialerBuilder("shadowsocks", shadowsocks.NewStreamDialer)
}
