package vpn

import (
	"fmt"
	"slices"
	"strings"
)

const splitTunnelTag = "split-tunnel"

// SplitTunnelPolicy selects whether matching traffic bypasses or uses the VPN.
type SplitTunnelPolicy string

const (
	// SplitTunnelPolicyExclude bypasses the VPN for matching apps and websites.
	SplitTunnelPolicyExclude SplitTunnelPolicy = "exclude"
	// SplitTunnelPolicyInclude routes only matching apps and websites through the VPN.
	SplitTunnelPolicyInclude SplitTunnelPolicy = "include"
)

// Valid reports whether p is a supported policy.
func (p SplitTunnelPolicy) Valid() bool {
	return p == SplitTunnelPolicyExclude || p == SplitTunnelPolicyInclude
}

// Filter type identifiers name SplitTunnelFilter fields for callers such as the CLI.
// They are defined here, not with the real implementation, so the novpn build exports them too.
const (
	TypeDomain           = "domain"
	TypeDomainSuffix     = "domainSuffix"
	TypeDomainKeyword    = "domainKeyword"
	TypeDomainRegex      = "domainRegex"
	TypeProcessName      = "processName"
	TypeProcessPath      = "processPath"
	TypeProcessPathRegex = "processPathRegex"
	TypePackageName      = "packageName"
)

// SplitTunnelState is the persisted policy and its selections.
type SplitTunnelState struct {
	Enabled bool              `json:"enabled"`
	Policy  SplitTunnelPolicy `json:"policy"`
	Filters SplitTunnelFilter `json:"filters"`
}

// SplitTunnelFilter selects websites and apps for split-tunnel routing.
type SplitTunnelFilter struct {
	Domain           []string
	DomainSuffix     []string
	DomainKeyword    []string
	DomainRegex      []string
	ProcessName      []string
	ProcessPath      []string
	ProcessPathRegex []string
	PackageName      []string
}

// String formats the nonempty selectors.
func (f SplitTunnelFilter) String() string {
	var str []string
	for _, field := range []struct {
		name   string
		values []string
	}{
		{TypeDomain, f.Domain}, {TypeDomainSuffix, f.DomainSuffix},
		{TypeDomainKeyword, f.DomainKeyword}, {TypeDomainRegex, f.DomainRegex},
		{TypeProcessName, f.ProcessName}, {TypeProcessPath, f.ProcessPath},
		{TypeProcessPathRegex, f.ProcessPathRegex}, {TypePackageName, f.PackageName},
	} {
		if len(field.values) > 0 {
			str = append(str, fmt.Sprintf("%s: %v", field.name, field.values))
		}
	}
	return "{" + strings.Join(str, ", ") + "}"
}

func (f SplitTunnelFilter) clone() SplitTunnelFilter {
	return SplitTunnelFilter{
		Domain: slices.Clone(f.Domain), DomainSuffix: slices.Clone(f.DomainSuffix),
		DomainKeyword: slices.Clone(f.DomainKeyword), DomainRegex: slices.Clone(f.DomainRegex),
		ProcessName: slices.Clone(f.ProcessName), ProcessPath: slices.Clone(f.ProcessPath),
		ProcessPathRegex: slices.Clone(f.ProcessPathRegex), PackageName: slices.Clone(f.PackageName),
	}
}

func (f *SplitTunnelFilter) update(diff SplitTunnelFilter, fn func([]string, []string) []string) {
	f.Domain = fn(f.Domain, diff.Domain)
	f.DomainSuffix = fn(f.DomainSuffix, diff.DomainSuffix)
	f.DomainKeyword = fn(f.DomainKeyword, diff.DomainKeyword)
	f.DomainRegex = fn(f.DomainRegex, diff.DomainRegex)
	f.ProcessName = fn(f.ProcessName, diff.ProcessName)
	f.ProcessPath = fn(f.ProcessPath, diff.ProcessPath)
	f.ProcessPathRegex = fn(f.ProcessPathRegex, diff.ProcessPathRegex)
	f.PackageName = fn(f.PackageName, diff.PackageName)
}
