//go:build !novpn

package vpn

import (
	"log/slog"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
)

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
