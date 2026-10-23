package vpn

import (
	"log/slog"
	"slices"

	O "github.com/sagernet/sing-box/option"

	"github.com/getlantern/radiance/internal/hostaccess"
	"github.com/getlantern/radiance/servers"
)

func allowedOutbounds(outbounds []O.Outbound) []O.Outbound {
	return slices.DeleteFunc(slices.Clone(outbounds), func(o O.Outbound) bool {
		return dropDenied("server", o.Tag, hostaccess.CheckOutbound(o))
	})
}

func allowedEndpoints(endpoints []O.Endpoint) []O.Endpoint {
	return slices.DeleteFunc(slices.Clone(endpoints), func(e O.Endpoint) bool {
		return dropDenied("server", e.Tag, hostaccess.CheckEndpoint(e))
	})
}

func allowedServices(services []O.Service) []O.Service {
	return slices.DeleteFunc(slices.Clone(services), func(s O.Service) bool {
		return dropDenied("service", s.Tag, hostaccess.CheckService(s))
	})
}

func allowedServers(list servers.ServerList) servers.ServerList {
	list.Servers = slices.DeleteFunc(slices.Clone(list.Servers), func(s *servers.Server) bool {
		return dropDenied("server", s.Tag, s.CheckHostAccess())
	})
	return list
}

func dropDenied(kind, tag string, err error) bool {
	if err == nil {
		return false
	}
	slog.Warn("Dropping "+kind+" whose options grant host access", "tag", tag, "error", err)
	return true
}
