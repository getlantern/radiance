//go:build !linux

package boxoptions

import "github.com/sagernet/sing-box/option"

func Options() option.Options {
	return BoxOptions
}
