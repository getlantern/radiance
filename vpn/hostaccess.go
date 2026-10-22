package vpn

import (
	"log/slog"
	"slices"

	O "github.com/sagernet/sing-box/option"

	"github.com/getlantern/radiance/internal/hostaccess"
	"github.com/getlantern/radiance/internal/tlsidentity"
	"github.com/getlantern/radiance/servers"
)

func allowedOutbounds(outbounds []O.Outbound) []O.Outbound {
	allowed := slices.DeleteFunc(slices.Clone(outbounds), func(o O.Outbound) bool {
		return dropDenied(o.Tag, hostaccess.CheckOutbound(o)) || dropDenied(o.Tag, tlsidentity.CheckUnbounded(o.Options))
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

func allowedServers(list servers.ServerList) servers.ServerList {
	list.Servers = slices.DeleteFunc(slices.Clone(list.Servers), func(s *servers.Server) bool {
		if dropDenied(s.Tag, s.CheckHostAccess()) {
			return true
		}
		outbound, ok := s.Options.(O.Outbound)
		return ok && dropDenied(s.Tag, tlsidentity.CheckUnbounded(outbound.Options))
	})
	for i, server := range list.Servers {
		list.Servers[i] = server.WithTLSVerification()
	}
	return list
}

func dropDenied(tag string, err error) bool {
	if err == nil {
		return false
	}
	slog.Warn("Dropping server with unsafe options", "tag", tag, "error", err)
	return true
}
