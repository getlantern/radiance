package backend

import (
	"slices"
	"strings"

	C "github.com/getlantern/common"
)

// withoutDirectRules returns rules without those that send their sites only
// to the direct outbound, leaving those sites to the rest of the routing: the
// proxyless hedge, or a proxy.
func withoutDirectRules(rules C.SmartRoutingRules) C.SmartRoutingRules {
	return slices.DeleteFunc(slices.Clone(rules), func(r C.SmartRoutingRule) bool {
		// Matches the outbounds the tunnel uses, which drops blank tags.
		direct := false
		for _, o := range r.Outbounds {
			switch strings.TrimSpace(o) {
			case "":
			case "direct":
				direct = true
			default:
				return false
			}
		}
		return direct
	})
}
