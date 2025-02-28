package algeneva

import (
	"net/netip"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/json/badoption"
)

var options = option.Options{
	Log: &option.LogOptions{
		Disabled:     false,
		Level:        "trace",
		Output:       "stdout",
		Timestamp:    true,
		DisableColor: true,
	},
	// DNS: &option.DNSOptions{
	// 	Servers: []option.DNSServerOptions{
	// 		{
	// 			Tag:     "dns-google",
	// 			Address: "8.8.8.8",
	// 		},
	// 		{
	// 			Tag:     "local",
	// 			Address: "223.5.5.5",
	// 			Detour:  "direct",
	// 		},
	// 	},
	// 	Rules: []option.DNSRule{
	// 		{
	// 			Type: "default",
	// 			DefaultOptions: option.DefaultDNSRule{
	// 				RawDefaultDNSRule: option.RawDefaultDNSRule{
	// 					Outbound: []string{"any"},
	// 				},
	// 				DNSRuleAction: option.DNSRuleAction{
	// 					Action: "route",
	// 					RouteOptions: option.DNSRouteActionOptions{
	// 						Server: "local",
	// 					},
	// 				},
	// 			},
	// 			LogicalOptions: option.LogicalDNSRule{},
	// 		},
	// 	},
	// },
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
				StrictRoute:            false,
				EndpointIndependentNat: true,
				Stack:                  "system",
			},
		},
		{
			Type: "algeneva",
			Tag:  "algeneva-in",
			Options: &AlgenevaInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     common.Ptr(badoption.Addr(netip.MustParseAddr("0.0.0.0"))),
					ListenPort: 8080,
				},
			},
		},
	},
	Outbounds: []option.Outbound{
		{
			Type: "direct",
			Tag:  "direct",
			// Options: &option.DirectOutboundOptions{},
		},
		// {
		// 	Type: "dns",
		// 	Tag:  "dns-out",
		// 	// Options: &option.DNSOptions{},
		// },
		{
			Type: "algeneva",
			Tag:  "algeneva-out",
			Options: &AlgenevaOutboundOptions{
				Strategy: "[HTTP:method:*]-insert{%0A:end:value:4}-|",
				ServerOptions: option.ServerOptions{
					Server:     "103.104.245.192",
					ServerPort: 80,
				},
			},
		},
		{
			Type: "algeneva",
			Tag:  "algeneva-local",
			Options: &AlgenevaOutboundOptions{
				Strategy: "[HTTP:method:*]-insert{%0A:end:value:4}-|",
				ServerOptions: option.ServerOptions{
					Server:     "127.0.0.1",
					ServerPort: 8080,
				},
			},
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
			{
				Type: "default",
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound:     badoption.Listable[string]{"tun-in"},
						Domain:      badoption.Listable[string]{"ipconfig.io"},
						ProcessName: badoption.Listable[string]{"curl"},
						ProcessPath: badoption.Listable[string]{"/usr/bin/curl"},
					},
					RuleAction: option.RuleAction{
						Action: "route",
						RouteOptions: option.RouteActionOptions{
							Outbound: "algeneva-out",
						},
					},
				},
			},
			{
				Type: "default",
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound:     badoption.Listable[string]{"tun-in"},
						Domain:      badoption.Listable[string]{"ipconfig.io"},
						ProcessName: badoption.Listable[string]{"curl"},
						ProcessPath: badoption.Listable[string]{"/usr/bin/curl"},
					},
					RuleAction: option.RuleAction{
						Action: "route",
						RouteOptions: option.RouteActionOptions{
							Outbound: "algeneva-out",
						},
					},
				},
			},
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
