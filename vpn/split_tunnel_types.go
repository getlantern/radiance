package vpn

import (
	"fmt"
	"slices"
	"strings"
)

// splitTunnelTag is the route rule-set tag the split-tunnel filter is bound to.
// It is shared across builds so the novpn build can reference the same tag when
// asserting the rule-set is absent.
const splitTunnelTag = "split-tunnel"

// SplitTunnelPolicy selects whether matching traffic bypasses or uses the VPN.
type SplitTunnelPolicy string

const (
	// SplitTunnelPolicyExclude makes matching apps/domains bypass the VPN and
	// routes everything else through it.
	SplitTunnelPolicyExclude SplitTunnelPolicy = "exclude"
	// SplitTunnelPolicyInclude routes only matching apps/domains through the VPN
	// and sends everything else direct.
	SplitTunnelPolicyInclude SplitTunnelPolicy = "include"
)

func (p SplitTunnelPolicy) Valid() bool {
	return p == SplitTunnelPolicyExclude || p == SplitTunnelPolicyInclude
}

// Filter type identifiers accepted by SplitTunnel.AddItems/RemoveItems and exposed
// through the backend and CLI. Defined here (rather than alongside the real
// implementation) so the novpn build, which compiles an inert SplitTunnel, still
// exports the same identifiers to callers.
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

// SplitTunnelFilter is the set of domains, processes, and packages a user has
// configured for split-tunnel routing.
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

func (f SplitTunnelFilter) String() string {
	var str []string
	if len(f.Domain) > 0 {
		str = append(str, fmt.Sprintf("domain: %v", f.Domain))
	}
	if len(f.DomainSuffix) > 0 {
		str = append(str, fmt.Sprintf("domainSuffix: %v", f.DomainSuffix))
	}
	if len(f.DomainKeyword) > 0 {
		str = append(str, fmt.Sprintf("domainKeyword: %v", f.DomainKeyword))
	}
	if len(f.DomainRegex) > 0 {
		str = append(str, fmt.Sprintf("domainRegex: %v", f.DomainRegex))
	}
	if len(f.ProcessName) > 0 {
		str = append(str, fmt.Sprintf("processName: %v", f.ProcessName))
	}
	if len(f.ProcessPath) > 0 {
		str = append(str, fmt.Sprintf("processPath: %v", f.ProcessPath))
	}
	if len(f.ProcessPathRegex) > 0 {
		str = append(str, fmt.Sprintf("processPathRegex: %v", f.ProcessPathRegex))
	}
	if len(f.PackageName) > 0 {
		str = append(str, fmt.Sprintf("packageName: %v", f.PackageName))
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
