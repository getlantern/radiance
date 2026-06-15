//go:build !novpn

package vpn

import (
	"log/slog"
	"net/netip"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common"
)

// baseInbounds returns the inbounds for a tunnel build: the TUN device plus the
// loopback bypass proxy used for kindling connections. The TUN inbound is at
// index 0 so applyPlatformTunnelOptions can adjust it per platform.
func baseInbounds() []O.Inbound {
	loopbackAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))

	// ULA-only interfaces don't indicate real public v6, so gate the TUN v6 address
	// on genuine global v6 connectivity.
	tunAddress := []netip.Prefix{
		netip.MustParsePrefix("10.10.1.1/30"),
	}
	if hasGlobalIPv6() {
		tunAddress = append(tunAddress, netip.MustParsePrefix("fdfe:dcba:9876::1/126"))
		slog.Info("vpn: TUN with IPv6 ULA (system has global v6)")
	} else {
		slog.Info("vpn: TUN IPv4-only (no global v6 detected)")
	}

	return []O.Inbound{
		{
			Type: "tun",
			Tag:  "tun-in",
			Options: &O.TunInboundOptions{
				InterfaceName:          "utun225",
				Address:                tunAddress,
				AutoRoute:              true,
				StrictRoute:            true,
				EndpointIndependentNat: true,     // needed for QUIC migration and hole-punching
				Stack:                  "system", // fallback to gvisor on older Android kernels in applyPlatformTunnelOptions
			},
		},
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

// applyPlatformTunnelOptions indexes opts.Inbounds[0] as the TUN device, per the
// invariant established by baseInbounds. Safe only in tunnel mode.
func applyPlatformTunnelOptions(opts *O.Options) {
	switch common.Platform {
	case "android":
		opts.Route.OverrideAndroidVPN = true
		kv := kernelVersion()
		slog.Debug("detected kernel version", "kernel", kv)
		if kv == "" {
			slog.Warn("kernel version unknown, keeping default TUN stack")
		} else if kernelBelow(kv, minAndroidSystemStackKernel) {
			opts.Inbounds[0].Options.(*O.TunInboundOptions).Stack = "gvisor"
			slog.Info("kernel below 5.10, using gvisor TUN stack", "kernel", kv)
		}
		slog.Debug("Android platform detected, OverrideAndroidVPN set to true")
	case "ios":
		opts.Inbounds[0].Options.(*O.TunInboundOptions).Stack = ""
		slog.Debug("iOS platform detected, using default TUN stack with no override")
	case "linux":
		opts.Inbounds[0].Options.(*O.TunInboundOptions).AutoRedirect = true
		slog.Debug("Linux platform detected, AutoRedirect set to true")
	}
}
