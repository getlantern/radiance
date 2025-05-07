package boxoptions

import (
	"net/netip"

	"github.com/sagernet/sing-box/constant"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	O "github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json/badoption"
)

const LanternAutoTag = "lantern-auto"

var (
	BoxOptions = O.Options{
		Log: &O.LogOptions{
			Disabled:     false,
			Level:        "trace",
			Output:       "lantern.log",
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
		// use direct as the default outbound for URLTest so sing-box starts
		Outbounds: append(BaseOutbounds, URLTestOutbound([]string{"direct"})),
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
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							Inbound: badoption.Listable[string]{"tun-in"},
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: O.RouteActionOptions{
								Outbound: LanternAutoTag,
							},
						},
					},
				},
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
	}
	BaseEndpoints = []O.Endpoint{}
)

func URLTestOutbound(tags []string) O.Outbound {
	return option.Outbound{
		Type: constant.TypeURLTest,
		Tag:  LanternAutoTag,
		Options: &option.URLTestOutboundOptions{
			Outbounds: tags,
		},
	}
}
