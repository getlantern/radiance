package boxoptions

import (
	"net/netip"
	"slices"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	O "github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json/badoption"
)

const (
	ServerGroupLantern = "lantern-servers"
	ServerGroupUser    = "user-servers"

	LanternAutoTag = "lantern-auto"
)

var (
	BoxOptions = O.Options{
		Log: &O.LogOptions{
			Disabled:     false,
			Level:        "debug",
			Output:       "lantern-box.log",
			Timestamp:    true,
			DisableColor: true,
		},
		DNS: &O.DNSOptions{
			Servers: []O.DNSServerOptions{
				{
					Tag:     "dns-google-dot",
					Address: "tls://8.8.4.4",
				},
				{
					Tag:     "dns-cloudflare-dot",
					Address: "tls://1.1.1.1",
				},
				{
					Tag:     "dns-sb-dot",
					Address: "tls://185.222.222.222",
				},
				{
					Tag:             "dns-google-doh",
					Address:         "https://dns.google/dns-query",
					AddressResolver: "dns-google-dot",
				},
				{
					Tag:             "dns-cloudflare-doh",
					Address:         "https://cloudflare-dns.com/dns-query",
					AddressResolver: "dns-cloudflare-dot",
				},
				{
					Tag:             "dns-sb-doh",
					Address:         "https://doh.dns.sb/dns-query",
					AddressResolver: "dns-sb-dot",
				},
				{
					Tag:     "local",
					Address: "223.5.5.5",
					Detour:  "direct",
				},
			},
			DNSClientOptions: O.DNSClientOptions{
				Strategy: O.DomainStrategy(dns.DomainStrategyUseIPv4),
			},
		},
		Inbounds: []O.Inbound{
			{
				Type: "tun",
				Tag:  "tun-in",
				Options: &O.TunInboundOptions{
					InterfaceName: "utun225",
					MTU:           1500,
					Address: badoption.Listable[netip.Prefix]{
						netip.MustParsePrefix("10.10.1.1/30"),
					},
					AutoRoute:   true,
					StrictRoute: true,
				},
			},
		},
		Endpoints: BaseEndpoints,
		Outbounds: BaseOutbounds,
		Route: &O.RouteOptions{
			AutoDetectInterface: true,
			Rules: []O.Rule{
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							Inbound: badoption.Listable[string]{"tun-in"},
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeSniff,
						},
					},
				},
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							Protocol: badoption.Listable[string]{"dns"},
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: O.RouteActionOptions{
								Outbound: "dns-out",
							},
						},
					},
				},
				{ // route to lantern servers
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							Inbound:   badoption.Listable[string]{"tun-in"},
							ClashMode: ServerGroupLantern,
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: O.RouteActionOptions{
								Outbound: ServerGroupLantern,
							},
						},
					},
				},
				{ // route to user servers
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							Inbound:   badoption.Listable[string]{"tun-in"},
							ClashMode: ServerGroupUser,
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: O.RouteActionOptions{
								Outbound: ServerGroupUser,
							},
						},
					},
				},
			},
		},
		Experimental: &O.ExperimentalOptions{
			ClashAPI: &O.ClashAPIOptions{
				DefaultMode:        ServerGroupLantern,
				ExternalController: "",
			},
		},
	}
	BaseOutbounds = []O.Outbound{
		{
			Type:    "direct",
			Tag:     "direct",
			Options: &O.DirectOutboundOptions{},
		},
		{
			Type:    "dns",
			Tag:     "dns-out",
			Options: &O.DNSOptions{},
		},
		{
			Type:    "block",
			Tag:     "block",
			Options: &O.StubOptions{},
		},
		SelectorOutbound([]string{LanternAutoTag}, ServerGroupLantern, LanternAutoTag),
		SelectorOutbound([]string{"direct"}, ServerGroupUser, "direct"),
		// use direct as the default outbound for URLTest so sing-box starts
		URLTestOutbound([]string{"direct"}),
	}
	BaseEndpoints = []O.Endpoint{}
)

var (
	permanentOutbounds = []string{
		"direct",
		"dns",
		"block",
		ServerGroupLantern,
		ServerGroupUser,
		LanternAutoTag,
	}
	permanentEndpoints = []string{}
)

func URLTestOutbound(tags []string) O.Outbound {
	return option.Outbound{
		Type: C.TypeURLTest,
		Tag:  LanternAutoTag,
		Options: &option.URLTestOutboundOptions{
			Outbounds: tags,
		},
	}
}

func SelectorOutbound(outbounds []string, group, defaultO string) O.Outbound {
	if !slices.Contains(outbounds, defaultO) {
		outbounds = append(outbounds, defaultO)
	}
	return option.Outbound{
		Type: C.TypeSelector,
		Tag:  group,
		Options: &option.SelectorOutboundOptions{
			Outbounds:                 outbounds,
			Default:                   defaultO,
			InterruptExistConnections: false,
		},
	}
}

func PermanentOutboundsEndpoints() ([]string, []string) {
	return append([]string{}, permanentOutbounds...), append([]string{}, permanentEndpoints...)
}
