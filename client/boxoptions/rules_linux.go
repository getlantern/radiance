package boxoptions

import (
	"github.com/sagernet/sing-box/option"
)

func Options() option.Options {
	opts := boxOptions
	for _, opt := range opts.Inbounds {
		if tun, ok := opt.Options.(*option.TunInboundOptions); ok {
			tun.AutoRedirect = true
		}
	}
	return opts
}
