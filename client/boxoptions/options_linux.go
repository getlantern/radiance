package boxoptions

import (
	"runtime"

	"github.com/sagernet/sing-box/option"
)

func Options(logOutput string) option.Options {
	opts := BoxOptions
	for _, opt := range opts.Inbounds {
		if tun, ok := opt.Options.(*option.TunInboundOptions); ok {
			tun.AutoRedirect = true
		}
	}

	if runtime.GOOS == "android" {
		opts.Route.OverrideAndroidVPN = true
	}
	opts.Log.Output = logOutput
	return opts
}
