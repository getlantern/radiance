//go:build !linux

package boxoptions

import "github.com/sagernet/sing-box/option"

func Options(logOutput string) option.Options {
	opts := BoxOptions
	opts.Log.Output = logOutput
	return opts
}
