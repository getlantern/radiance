package client

import (
	"net/netip"

	"github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json/badoption"
)

var boxOptions = option.Options{
	Log: &option.LogOptions{
		Disabled:     false,
		Level:        "trace",
		Output:       "lantern.log",
		Timestamp:    true,
		DisableColor: true,
	},
	DNS: &option.DNSOptions{
		Servers: []option.DNSServerOptions{
			{
				Tag:     "dns-google",
				Address: "8.8.8.8",
			},
		},
		Rules: []option.DNSRule{
			{
				Type: "default",
				DefaultOptions: option.DefaultDNSRule{
					RawDefaultDNSRule: option.RawDefaultDNSRule{
						Outbound: []string{"any"},
					},
					DNSRuleAction: option.DNSRuleAction{
						Action: "route",
						RouteOptions: option.DNSRouteActionOptions{
							Server: "dns-google",
						},
					},
				},
				LogicalOptions: option.LogicalDNSRule{},
			},
		},
		DNSClientOptions: option.DNSClientOptions{
			Strategy: option.DomainStrategy(dns.DomainStrategyUseIPv4),
		},
	},
	Inbounds: []option.Inbound{
		{
			Type: "tun",
			Tag:  "tun-in",
			Options: &option.TunInboundOptions{
				InterfaceName: "utun225",
				MTU:           1500,
				Address: badoption.Listable[netip.Prefix]{
					netip.MustParsePrefix("10.10.1.1/30"),
				},
				AutoRoute:              true,
				AutoRedirect:           true,
				StrictRoute:            true,
				EndpointIndependentNat: true,
				Stack:                  "system",
			},
		},
	},
	Outbounds: []option.Outbound{
		{
			Type: "direct",
			Tag:  "direct",
			// Options: &option.DirectOutboundOptions{},
		},
		{
			Type: "dns",
			Tag:  "dns-out",
			// Options: &option.DNSOptions{},
		},
		{
			Type: "http",
			Tag:  "sing-out",
			Options: &option.HTTPOutboundOptions{
				ServerOptions: option.ServerOptions{
					Server:     "103.104.245.192",
					ServerPort: 80,
				},
			},
		},
	},
	Route: &option.RouteOptions{
		AutoDetectInterface: true,
		Rules: []option.Rule{
			{
				Type: "default",
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{"tun-in"},
					},
					RuleAction: option.RuleAction{
						Action: "sniff",
					},
				},
			},
			// {
			// 	Type: "default",
			// 	DefaultOptions: option.DefaultRule{
			// 		RawDefaultRule: option.RawDefaultRule{
			// 			Inbound:     badoption.Listable[string]{"tun-in"},
			// 			Domain:      badoption.Listable[string]{"ipconfig.io"},
			// 			ProcessName: badoption.Listable[string]{"curl"},
			// 			ProcessPath: badoption.Listable[string]{"/usr/bin/curl"},
			// 		},
			// 		RuleAction: option.RuleAction{
			// 			Action: "route",
			// 			RouteOptions: option.RouteActionOptions{
			// 				Outbound: "algeneva-out",
			// 			},
			// 		},
			// 	},
			// },
			// {
			// 	Type: "default",
			// 	DefaultOptions: option.DefaultRule{
			// 		RawDefaultRule: option.RawDefaultRule{
			// 			Protocol: badoption.Listable[string]{"dns"},
			// 		},
			// 		RuleAction: option.RuleAction{
			// 			Action: "route",
			// 			RouteOptions: option.RouteActionOptions{
			// 				Outbound: "dns-out",
			// 			},
			// 		},
			// 	},
			// },
		},
	},
}
