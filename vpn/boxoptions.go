package vpn

import (
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
)

const (
	ServerGroupLantern = "lantern"
	ServerGroupUser    = "user"

	modeAutoLantern = "auto-lantern"
	modeAutoUser    = "auto-user"
	modeAutoAll     = "auto-all"

	modeBlock = "block"
)

// serverOptions is a helper struct to iterate over the outbounds and endpoints
type serverOptions struct {
	outbounds []O.Outbound
	endpoints []O.Endpoint
}

func (s *serverOptions) tags() []string {
	tags := make([]string, 0, len(s.outbounds)+len(s.endpoints))
	for _, ob := range s.outbounds {
		tags = append(tags, ob.Tag)
	}
	for _, ep := range s.endpoints {
		tags = append(tags, ep.Tag)
	}
	return tags
}

func newSelector(group string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: C.TypeSelector,
		Tag:  group,
		Options: &O.SelectorOutboundOptions{
			Outbounds:                 outbounds,
			InterruptExistConnections: false,
		},
	}
}

func buildOptions(mode string) (O.Options, error) {
	return O.Options{}, nil
}
