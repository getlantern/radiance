package client

import (
	"github.com/getlantern/radiance/outbounds/proxyless"
	"github.com/sagernet/sing-box/adapter/outbound"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[AlgenevaOutboundOptions](registry, "algeneva", NewOutbound)
	outbound.Register[proxyless.ProxylessOutboundOptions](registry, "proxyless", proxyless.NewProxylessOutbound)
}
