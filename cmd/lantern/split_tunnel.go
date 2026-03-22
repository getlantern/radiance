package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/ipc"
	"github.com/getlantern/radiance/vpn"
)

type SplitTunnelCmd struct {
	Enable *bool  `arg:"positional" help:"enable or disable split tunneling (true|false)"`
	List   bool   `arg:"-l,--list" help:"list current filters"`
	Add    string `arg:"--add" help:"add filter (TYPE:VALUE, e.g. domain-suffix:example.com)"`
	Remove string `arg:"--remove" help:"remove filter (TYPE:VALUE)"`
}

func runSplitTunnel(ctx context.Context, c *ipc.Client, cmd *SplitTunnelCmd) error {
	switch {
	case cmd.Add != "":
		typ, val, err := parseFilter(cmd.Add)
		if err != nil {
			return err
		}
		return c.AddSplitTunnelItems(ctx, buildFilter(typ, val))
	case cmd.Remove != "":
		typ, val, err := parseFilter(cmd.Remove)
		if err != nil {
			return err
		}
		return c.RemoveSplitTunnelItems(ctx, buildFilter(typ, val))
	case cmd.List:
		return splitTunnelList(ctx, c)
	case cmd.Enable != nil:
		if err := c.EnableSplitTunneling(ctx, *cmd.Enable); err != nil {
			return err
		}
		fmt.Printf("Split tunneling set to %v\n", *cmd.Enable)
		return nil
	default:
		return splitTunnelStatus(ctx, c)
	}
}

func splitTunnelStatus(ctx context.Context, c *ipc.Client) error {
	s, err := c.Settings(ctx)
	if err != nil {
		return err
	}
	v := s[settings.SplitTunnelKey]
	if v == nil {
		v = false
	}
	fmt.Printf("Split tunneling: %v\n", v)
	return nil
}

func splitTunnelList(ctx context.Context, c *ipc.Client) error {
	s, err := c.Settings(ctx)
	if err != nil {
		return err
	}
	fmt.Println("Enabled:", s[settings.SplitTunnelKey])
	filters, err := c.SplitTunnelFilters(ctx)
	if err != nil {
		return err
	}
	fmt.Println(filters.String())
	return nil
}

// parseFilter splits "TYPE:VALUE" into the internal filter type and value.
func parseFilter(spec string) (string, string, error) {
	typ, val, ok := strings.Cut(spec, ":")
	if !ok || val == "" {
		return "", "", fmt.Errorf("filter format: TYPE:VALUE (e.g. domain-suffix:example.com)")
	}
	return filterTypeFromArg(typ), val, nil
}

// filterTypeFromArg converts a CLI arg like "domain-suffix" to the internal type "domainSuffix".
func filterTypeFromArg(a string) string {
	s, rest, _ := strings.Cut(a, "-")
	if rest != "" {
		s += strings.ToUpper(rest[:1]) + rest[1:]
	}
	return s
}

func buildFilter(filterType, value string) vpn.SplitTunnelFilter {
	var f vpn.SplitTunnelFilter
	switch filterType {
	case vpn.TypeDomain:
		f.Domain = []string{value}
	case vpn.TypeDomainSuffix:
		f.DomainSuffix = []string{value}
	case vpn.TypeDomainKeyword:
		f.DomainKeyword = []string{value}
	case vpn.TypeDomainRegex:
		f.DomainRegex = []string{value}
	case vpn.TypeProcessName:
		f.ProcessName = []string{value}
	case vpn.TypeProcessPath:
		f.ProcessPath = []string{value}
	case vpn.TypeProcessPathRegex:
		f.ProcessPathRegex = []string{value}
	case vpn.TypePackageName:
		f.PackageName = []string{value}
	}
	return f
}
