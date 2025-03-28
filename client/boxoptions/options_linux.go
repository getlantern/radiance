package boxoptions

import (
	"runtime"

	"github.com/sagernet/sing-box/option"
)

func Options(dataDir, logOutput string) option.Options {
	opts := boxOptions(dataDir, logOutput)
	for _, opt := range opts.Inbounds {
		if tun, ok := opt.Options.(*option.TunInboundOptions); ok {
			if runtime.GOOS != "android" {
				tun.AutoRedirect = true
			}

		}
	}

	if runtime.GOOS == "android" {
		opts.Route.OverrideAndroidVPN = true
	}
	return opts
}
