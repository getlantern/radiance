package vpn

import (
	"log/slog"
	"slices"

	lcommon "github.com/getlantern/common"
	O "github.com/sagernet/sing-box/option"

	"github.com/getlantern/radiance/internal/hostaccess"
	"github.com/getlantern/radiance/internal/tlsidentity"
	"github.com/getlantern/radiance/servers"
)

func allowedOutbounds(outbounds []O.Outbound) []O.Outbound {
	allowed := slices.DeleteFunc(slices.Clone(outbounds), func(o O.Outbound) bool {
		return dropDenied(o.Tag, hostaccess.CheckOutbound(o)) || dropDenied(o.Tag, tlsidentity.CheckCustomOptions(o.Options))
	})
	for i := range allowed {
		allowed[i].Options = tlsidentity.Options(allowed[i].Options)
	}
	return allowed
}

func allowedEndpoints(endpoints []O.Endpoint) []O.Endpoint {
	allowed := slices.DeleteFunc(slices.Clone(endpoints), func(e O.Endpoint) bool {
		return dropDenied(e.Tag, hostaccess.CheckEndpoint(e))
	})
	for i := range allowed {
		allowed[i].Options = tlsidentity.Options(allowed[i].Options)
	}
	return allowed
}

func allowedServers(list servers.ServerList, retainedTags ...string) servers.ServerList {
	list, _ = NormalizeServers(list, nil, retainedTags...)
	return list
}

// NormalizeServers enforces proxy policy and removes missing dependencies without modifying its inputs.
// Retained tags identify dependencies supplied outside the list.
func NormalizeServers(list servers.ServerList, rules lcommon.SmartRoutingRules, retainedTags ...string) (servers.ServerList, lcommon.SmartRoutingRules) {
	options, rules := normalizeOutbounds(O.Options{Outbounds: list.Outbounds(), Endpoints: list.Endpoints()}, rules, retainedTags...)
	normalized := make(map[string]any, len(options.Outbounds)+len(options.Endpoints))
	for _, out := range options.Outbounds {
		normalized[out.Tag] = out
	}
	for _, endpoint := range options.Endpoints {
		normalized[endpoint.Tag] = endpoint
	}
	allowed := make([]*servers.Server, 0, len(normalized))
	for _, server := range list.Servers {
		if options, ok := normalized[server.Tag]; ok {
			copy := *server
			copy.Options = options
			allowed = append(allowed, &copy)
		}
	}
	list.Servers = allowed
	return list, rules
}

// ServerDependencies returns the outbound tags referenced by the servers in list.
func ServerDependencies(list servers.ServerList) []string {
	var tags []string
	for _, srv := range list.Servers {
		switch opts := srv.Options.(type) {
		case O.Outbound:
			tags = append(tags, outboundDependencies(opts.Options)...)
		case O.Endpoint:
			tags = append(tags, outboundDependencies(opts.Options)...)
		}
	}
	return tags
}

func dropDenied(tag string, err error) bool {
	if err == nil {
		return false
	}
	slog.Warn("Dropping server with unsafe options", "tag", tag, "error", err)
	return true
}
