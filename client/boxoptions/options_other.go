//go:build !linux

package boxoptions

import "github.com/sagernet/sing-box/option"

func Options(dataDir, logOutput string) option.Options {
	opts := boxOptions(dataDir, logOutput)
	return opts
}
