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
		return dropDenied(o.Tag, hostaccess.CheckOutbound(o))
	})
}

func allowedEndpoints(endpoints []O.Endpoint) []O.Endpoint {
	return slices.DeleteFunc(slices.Clone(endpoints), func(e O.Endpoint) bool {
		return dropDenied(e.Tag, hostaccess.CheckEndpoint(e))
	})
}

func allowedServers(list servers.ServerList) servers.ServerList {
	list.Servers = slices.DeleteFunc(slices.Clone(list.Servers), func(s *servers.Server) bool {
		return dropDenied(s.Tag, s.CheckHostAccess())
	})
	return list
}

func dropDenied(tag string, err error) bool {
	if err == nil {
		return false
	}
	slog.Warn("Dropping server whose options grant host access", "tag", tag, "error", err)
	return true
}
