//go:build novpn

package vpn

import (
	"net/netip"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/getlantern/radiance/bypass"
)

// baseInbounds returns only the loopback bypass proxy. The novpn build has no TUN
// device; the user-facing inbound is the SOCKS/HTTP proxy added in buildOptions
// when RADIANCE_USE_SOCKS_PROXY is set.
func baseInbounds() []O.Inbound {
	loopbackAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	return []O.Inbound{
		{
			Type: C.TypeMixed,
			Tag:  bypass.BypassInboundTag,
			Options: &O.HTTPMixedInboundOptions{
				ListenOptions: O.ListenOptions{
					Listen:     &loopbackAddr,
					ListenPort: bypass.ProxyPort,
				},
			},
		},
	}
}

func applyPlatformTunnelOptions(*O.Options) {}
