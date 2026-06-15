//go:build novpn

package vpn

import "github.com/getlantern/radiance/common/env"

// socksOnlyEnforced forces SOCKS-proxy mode for the novpn build, which has no TUN
// device. A missing RADIANCE_SOCKS_ADDRESS is unrecoverable misconfiguration for
// this build, so it panics rather than starting a tunnel that can never carry
// traffic.
func socksOnlyEnforced() bool {
	if addr, ok := env.Get(env.SocksAddress); !ok || addr == "" {
		panic("novpn build requires " + env.SocksAddress.String() + " to be set")
	}
	env.Set(env.UseSocks.String(), "true")
	return true
}
