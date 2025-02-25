package client

import (
	"github.com/getlantern/radiance/outbounds/proxyless"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/option"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[AlgenevaOutboundOptions](registry, "algeneva", NewOutbound)
	outbound.Register[option.DialerOptions](registry, "proxyless", proxyless.NewProxylessOutbound)
}
