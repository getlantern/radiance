//go:build !novpn

package vpn

import (
	"log/slog"
	"net/netip"
	"strconv"
	"strings"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common"
)

const (
	// minAndroidSystemStackKernel is the minimum Linux kernel version (major.minor)
	// required for the system TUN stack to work reliably on Android only. Devices
	// running a kernel below this version fall back to gvisor. This constant has no
	// effect on other platforms.
	minAndroidSystemStackKernel = "5.10"

	inboundTag = "tun-in"
)

// baseInbounds returns the tunnel-build inbounds: the TUN device (per-platform
// stack/routing applied here) plus the loopback bypass proxy that carries
// kindling's bootstrap traffic outside the tunnel.
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

	tunOpts := &O.TunInboundOptions{
		InterfaceName:          "utun225",
		Address:                tunAddress,
		AutoRoute:              true,
		StrictRoute:            true,
		EndpointIndependentNat: true, // needed for QUIC migration and hole-punching
		Stack:                  "system",
	}
	switch common.Platform {
	case "android":
		kv := kernelVersion()
		slog.Debug("detected kernel version", "kernel", kv)
		if kv == "" {
			slog.Warn("kernel version unknown, keeping default TUN stack")
		} else if kernelBelow(kv, minAndroidSystemStackKernel) {
			tunOpts.Stack = "gvisor"
			slog.Info("kernel below 5.10, using gvisor TUN stack", "kernel", kv)
		}
	case "ios":
		tunOpts.Stack = ""
	case "linux":
		tunOpts.AutoRedirect = true
	}

	return []O.Inbound{
		{
			Type:    "tun",
			Tag:     inboundTag,
			Options: tunOpts,
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

// bypassRoutingRules routes kindling's bypass-proxy traffic directly, keeping it
// out of the tunnel it bootstraps.
func bypassRoutingRules() []O.Rule {
	return []O.Rule{
		{
			Type: C.RuleTypeDefault,
			DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{
					Inbound: []string{bypass.BypassInboundTag},
				},
				RuleAction: O.RuleAction{
					Action: C.RuleActionTypeRoute,
					RouteOptions: O.RouteActionOptions{
						Outbound: "direct",
					},
				},
			},
		},
	}
}

// splitTunnelRuleSet has the side effect of creating the rule file on disk if absent.
func splitTunnelRuleSet(basePath string) []O.RuleSet {
	splitTunnelPath := newSplitTunnel(basePath, slog.Default()).ruleFile
	return []O.RuleSet{
		{
			Type: C.RuleSetTypeLocal,
			Tag:  splitTunnelTag,
			LocalOptions: O.LocalRuleSet{
				Path: splitTunnelPath,
			},
			Format: C.RuleSetFormatSource,
		},
	}
}

func splitTunnelRoutingRules() []O.Rule {
	return []O.Rule{
		{
			Type: C.RuleTypeDefault,
			DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{
					RuleSet: []string{splitTunnelTag},
				},
				RuleAction: O.RuleAction{
					Action: C.RuleActionTypeRoute,
					RouteOptions: O.RouteActionOptions{
						Outbound: "direct",
					},
				},
			},
		},
	}
}

// kernelBelow reports whether the kernel version string v is below min.
// Only the first two components (major.minor) are compared, e.g. "5.10" or "4.19.0-android13".
// Returns false if either version string cannot be parsed.
func kernelBelow(v, min string) bool {
	parseKernelMajorMinor := func(s string) (int, int, bool) {
		p := strings.SplitN(s, ".", 3)
		if len(p) < 2 {
			return 0, 0, false
		}
		// Strip non-numeric suffixes (e.g. "19" from "19-android13")
		numericPrefix := func(part string) string {
			for i, r := range part {
				if r < '0' || r > '9' {
					return part[:i]
				}
			}
			return part
		}
		majorStr := numericPrefix(p[0])
		minorStr := numericPrefix(p[1])
		if majorStr == "" || minorStr == "" {
			return 0, 0, false
		}
		major, err := strconv.Atoi(majorStr)
		if err != nil {
			return 0, 0, false
		}
		minor, err := strconv.Atoi(minorStr)
		if err != nil {
			return 0, 0, false
		}
		return major, minor, true
	}
	vMaj, vMin, vok := parseKernelMajorMinor(v)
	mMaj, mMin, mok := parseKernelMajorMinor(min)
	if !vok || !mok {
		return false
	}
	return vMaj < mMaj || (vMaj == mMaj && vMin < mMin)
}
