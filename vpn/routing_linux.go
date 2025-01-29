package vpn

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// This is based on the routing code from the outline-cli example in the Outline-SDK
// https://github.com/Jigsaw-Code/outline-sdk/blob/main/x/examples/outline-cli/routing_linux.go

const (
	tableID       = 252
	tablePriority = 25552
)

// startRouting configures the routing table and IP rule to forward packets to the TUN interface.
func startRouting(rConf *RoutingConfig, proxyAddr string, bypassUDP bool) error {
	err := configureRoutingTable(tableID, rConf)
	if err != nil {
		err = fmt.Errorf("could not configure routing table: %w", err)
		log.Error(err)
		return err
	}
	if err = addIPRule(proxyAddr+"/32", bypassUDP); err != nil {
		err = fmt.Errorf("could not configure IP rule: %w", err)
		log.Error(err)
		return err
	}
	return nil
}

// stopRouting removes the routing table and IP rule that forwards packets to the TUN interface.
func stopRouting(rConf *RoutingConfig) error {
	log.Debug("removing routing rules")
	if err := deleteRoutingTable(tableID); err != nil {
		err = fmt.Errorf("failed to delete routing table: %w", err)
		log.Error(err)
		return err
	}
	if err := deleteIPRule(); err != nil {
		err = fmt.Errorf("failed to delete IP rule: %w", err)
		log.Error(err)
		return err
	}
	return nil
}

// configureRoutingTable adds routes to the routing table, tableID,
func configureRoutingTable(tableID int, rConf *RoutingConfig) error {
	ifce, err := netlink.LinkByName(rConf.TunName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %w", rConf.TunName, err)
	}

	dst, err := netlink.ParseIPNet(rConf.Gw)
	if err != nil {
		return fmt.Errorf("failed to parse gateway '%s': %w", rConf.Gw, err)
	}

	route := netlink.Route{
		LinkIndex: ifce.Attrs().Index,
		Table:     tableID,
		Dst:       dst,
		Src:       net.ParseIP(rConf.TunIP),
		Scope:     netlink.SCOPE_LINK,
	}
	if err := netlink.RouteAdd(&route); err != nil {
		return fmt.Errorf("failed to add route %v -> %v: %w", route.Src, route.Dst, err)
	}
	log.Debugf("Added routing table: %v -> %v", route.Src, route.Dst)

	// add a default route to the routing table to forward packets to the gateway
	route = netlink.Route{
		LinkIndex: ifce.Attrs().Index,
		Table:     tableID,
		Gw:        dst.IP,
	}
	if err := netlink.RouteAdd(&route); err != nil {
		return fmt.Errorf("failed to add default route for gateway %v: %w", route.Gw, err)
	}
	log.Debugf("routing through gateway %v on %v", route.Gw, route.LinkIndex)

	return nil
}

// deleteRoutingTable deletes all routes in the routing table.
func deleteRoutingTable(tableID int) error {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{
		Table: tableID,
	}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("failed to get routes in table %d: %v", tableID, err)
	}

	errs := []error{}
	for _, route := range routes {
		log.Debugf("Deleting route: %v", route)
		if err := netlink.RouteDel(&route); err != nil {
			errs = append(errs, fmt.Errorf("%v: %w", route, err))
		}
	}
	err = errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("failed to delete routes: %w", err)
	}

	log.Debugf("Deleted routing table: %v", tableID)
	return nil
}

// Store the IP rule so we can delete it later. This way, we don't accidentally delete other IP
// rules, which could cause major network issues (again XD). We could try to filter for it, but this
// is was safer!
var ipRule *netlink.Rule

// addIPRule adds an IP rule to forward packets to the TUN interface.
func addIPRule(proxyAddr string, bypassUDP bool) error {
	dst, err := netlink.ParseIPNet(proxyAddr)
	if err != nil {
		return fmt.Errorf("invalid IP address %s: %w", proxyAddr, err)
	}

	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Table = tableID
	rule.Priority = tablePriority
	rule.Dst = dst
	rule.Invert = true
	if bypassUDP {
		rule.IPProto = unix.IPPROTO_TCP
	}

	if err := netlink.RuleAdd(rule); err != nil {
		return fmt.Errorf("failed to add IP rule: %w", err)
	}
	ipRule = rule
	log.Debugf("Added IP rule: %v", rule)
	return nil
}

// deleteIPRule deletes the IP rule that forwards packets to the TUN interface.
func deleteIPRule() error {
	if ipRule == nil {
		return nil
	}

	if err := netlink.RuleDel(ipRule); err != nil {
		return fmt.Errorf("failed to delete IP rule: %w", err)
	}

	log.Debugf("Deleted IP rule: %v", ipRule)
	ipRule = nil
	return nil
}
