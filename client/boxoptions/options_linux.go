package boxoptions

import (
	"runtime"

	"github.com/sagernet/sing-box/option"
)

func Options() option.Options {
	opts := boxOptions
	for _, opt := range opts.Inbounds {
		if tun, ok := opt.Options.(*option.TunInboundOptions); ok {
			tun.AutoRedirect = true
		}
	}

	if runtime.GOOS == "android" {
		opts.Route.OverrideAndroidVPN = true
	}
	return opts
}
