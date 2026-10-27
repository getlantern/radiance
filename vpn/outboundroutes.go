package vpn

import (
	"slices"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
)

func routeOutboundTags(route *O.RouteOptions) []string {
	if route == nil {
		return nil
	}
	var tags []string
	if route.Final != "" {
		tags = append(tags, route.Final)
	}
	for _, rule := range route.Rules {
		var action O.RuleAction
		switch rule.Type {
		case "", C.RuleTypeDefault:
			action = rule.DefaultOptions.RuleAction
		case C.RuleTypeLogical:
			action = rule.LogicalOptions.RuleAction
		default:
			continue
		}
		if tag := routeActionOutboundTag(action); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func routeActionOutboundTag(action O.RuleAction) string {
	switch action.Action {
	case "", C.RuleActionTypeRoute:
		return action.RouteOptions.Outbound
	case C.RuleActionTypeBypass:
		return action.BypassOptions.Outbound
	default:
		return ""
	}
}

// rejectMissingOutboundRoutes replaces routes to missing targets with rejects,
// preserving match conditions, order, and the input.
func rejectMissingOutboundRoutes(rules []O.Rule, available map[string]bool) []O.Rule {
	rules = slices.Clone(rules)
	for i := range rules {
		var action *O.RuleAction
		switch rules[i].Type {
		case "", C.RuleTypeDefault:
			action = &rules[i].DefaultOptions.RuleAction
		case C.RuleTypeLogical:
			action = &rules[i].LogicalOptions.RuleAction
		default:
			continue
		}
		if tag := routeActionOutboundTag(*action); tag != "" && !available[tag] {
			*action = O.RuleAction{Action: C.RuleActionTypeReject}
		}
	}
	return rules
}
