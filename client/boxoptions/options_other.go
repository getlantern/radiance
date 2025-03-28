//go:build !linux

package boxoptions

import "github.com/sagernet/sing-box/option"

func Options(dataDir, logOutput, splitTunnelTag, splitTunnelFormat string) option.Options {
	opts := boxOptions(dataDir, logOutput, splitTunnelTag, splitTunnelFormat)
	return opts
}
