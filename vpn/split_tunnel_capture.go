package vpn

import (
	"slices"

	O "github.com/sagernet/sing-box/option"
)

// Android's native bridge treats @none as an empty allowlist, which must not become capture-all.
const emptyAndroidIncludePackage = "@none"

func androidSplitTunnelCapture(state SplitTunnelState) (include, exclude []string) {
	if !state.Enabled {
		return nil, nil
	}
	if state.Policy == SplitTunnelPolicyExclude {
		return nil, slices.Clone(state.Filters.PackageName)
	}
	f := state.Filters
	if len(f.Domain)+len(f.DomainSuffix)+len(f.DomainKeyword)+len(f.DomainRegex)+
		len(f.ProcessName)+len(f.ProcessPath)+len(f.ProcessPathRegex) > 0 {
		return nil, nil
	}
	if len(f.PackageName) == 0 {
		return []string{emptyAndroidIncludePackage}, nil
	}
	return slices.Clone(f.PackageName), nil
}

func configureSplitTunnelCapture(inbounds []O.Inbound, state SplitTunnelState, platform string) {
	if platform != "android" {
		return
	}
	include, exclude := androidSplitTunnelCapture(state)
	for _, inbound := range inbounds {
		if tun, ok := inbound.Options.(*O.TunInboundOptions); ok {
			tun.IncludePackage = include
			tun.ExcludePackage = exclude
		}
	}
}

func splitTunnelCaptureChanged(before, after SplitTunnelState, platform string) bool {
	if platform != "android" {
		return false
	}
	previousInclude, previousExclude := androidSplitTunnelCapture(before)
	nextInclude, nextExclude := androidSplitTunnelCapture(after)
	return !slices.Equal(previousInclude, nextInclude) || !slices.Equal(previousExclude, nextExclude)
}
