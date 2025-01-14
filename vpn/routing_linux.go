package vpn

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// This is based on the routing code from the outline-cli example in the Outline-SDK
// https://github.com/Jigsaw-Code/outline-sdk/blob/main/x/examples/outline-cli/routing_linux.go

const (
	tableID       = 1337
	tablePriority = 133337
)

type routingConfig struct {
	ifceName        string
	ifceIP          string
	ifceGatewayCIDR string
	tableID         int
	tablePriority   int
}

// startRouting configures the routing table and IP rule to forward packets to the TUN interface.
func startRouting(proxyAddr string, routeConf routingConfig) error {
	err := configureRoutingTable(routeConf.tableID, routeConf.ifceName, routeConf.ifceGatewayCIDR, routeConf.ifceIP)
	if err != nil {
		return fmt.Errorf("failed to configure routing table: %w", err)
	}
	return configureIPRule(proxyAddr, routeConf.tableID, routeConf.tablePriority)
}

// stopRouting removes the routing table and IP rule that forwards packets to the TUN interface.
func stopRouting(tableID int) error {
	if err := deleteRoutingTable(tableID); err != nil {
		return fmt.Errorf("failed to delete routing table: %w", err)
	}
	return deleteIPRule(tableID)
}

// configureRoutingTable adds routes to the routing table, tableID, to forward packets to the TUN
// interface, ifceName, with the gateway, gateway, and IP address, ifceIP.
func configureRoutingTable(tableID int, ifceName, gateway, ifceIP string) error {
	ifce, err := netlink.LinkByName(ifceName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %w", ifceName, err)
	}

	dst, err := netlink.ParseIPNet(gateway)
	if err != nil {
		return fmt.Errorf("invalid gateway IP address %s: %w", gateway, err)
	}

	route := netlink.Route{
		LinkIndex: ifce.Attrs().Index,
		Table:     tableID,
		Dst:       dst,
		Src:       net.ParseIP(ifceIP),
		Scope:     netlink.SCOPE_LINK,
	}
	if err := netlink.RouteAdd(&route); err != nil {
		return fmt.Errorf("failed to add routing table: %w", err)
	}
	log.Debugf("Added routing table: %v", route)

	// add a default route to the routing table to forward packets to the gateway
	route = netlink.Route{
		LinkIndex: ifce.Attrs().Index,
		Table:     tableID,
		Gw:        dst.IP,
	}
	if err := netlink.RouteAdd(&route); err != nil {
		return fmt.Errorf("failed to add gateway routing table: %w", err)
	}
	log.Debugf("Added gateway routing table: %v", route)

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

	for _, route := range routes {
		if err := netlink.RouteDel(&route); err != nil {
			return fmt.Errorf("failed to delete route %+v: %w", route, err)
		}
	}

	log.Debugf("Deleted routing table: %v", tableID)
	return nil
}

// configureIPRule adds an IP rule to forward packets to the TUN interface.
func configureIPRule(dstIP string, table, priority int) error {
	dst, err := netlink.ParseIPNet(dstIP)
	if err != nil {
		return fmt.Errorf("invalid IP address %s: %w", dstIP, err)
	}

	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Table = table
	rule.Priority = priority
	rule.Dst = dst
	rule.Invert = true

	if err := netlink.RuleAdd(rule); err != nil {
		return fmt.Errorf("failed to add IP rule: %w", err)
	}
	log.Debugf("Added IP rule: %v", rule)
	return nil
}

// deleteIPRule deletes the IP rule that forwards packets to the TUN interface.
func deleteIPRule(table int) error {
	rule := netlink.Rule{
		Table: table,
	}
	rules, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &rule, 0)
	if err != nil {
		return fmt.Errorf("failed to get IP rules: %w", err)
	}
	if len(rules) == 0 {
		return fmt.Errorf("no IP rules found for table %d", table)
	}

	// the table should only have one rule since we only added one
	rule = rules[0]
	if err := netlink.RuleDel(&rule); err != nil {
		return fmt.Errorf("failed to delete IP rule: %w", err)
	}

	log.Debugf("Deleted IP rule: %v", rule)
	return nil
}
