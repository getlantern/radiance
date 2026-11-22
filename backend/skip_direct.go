package backend

import (
	"slices"

	C "github.com/getlantern/common"
)

// withoutDirectRules returns rules without those that send their sites only
// to the direct outbound, leaving those sites to the rest of the routing: the
// proxyless hedge, or a proxy.
func withoutDirectRules(rules C.SmartRoutingRules) C.SmartRoutingRules {
	return slices.DeleteFunc(slices.Clone(rules), func(r C.SmartRoutingRule) bool {
		return len(r.Outbounds) > 0 && !slices.ContainsFunc(r.Outbounds, func(o string) bool { return o != "direct" })
	})
}
