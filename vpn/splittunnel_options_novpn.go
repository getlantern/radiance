//go:build novpn

package vpn

import O "github.com/sagernet/sing-box/option"

func splitTunnelRuleSet(_ string) []O.RuleSet { return nil }

func splitTunnelRoutingRules() []O.Rule { return nil }
