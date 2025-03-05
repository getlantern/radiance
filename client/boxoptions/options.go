package boxoptions

import (
	"net/netip"
	"time"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json/badoption"
)

var testTimeout = 10 * time.Second

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
				Tag:     "dns-google-dot",
				Address: "tls://8.8.8.8",
			},
			{
				Tag:     "local",
				Address: "223.5.5.5",
				Detour:  "direct",
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
							Server: "dns-google-dot",
						},
					},
				},
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
			Type: constant.TypeOutline,
			Tag:  "outline-out",
			Options: &option.OutboundOutlineOptions{
				DNSResolvers: []option.DNSEntryConfig{
					{
						TLS: &option.TLSEntryConfig{
							Name:    "dns.google",
							Address: "8.8.8.8:853",
						},
					},
					{
						System: &struct{}{},
					},
				},
				TLS:         []string{"", "split:1"},
				Domains:     []string{"google.com"},
				TestTimeout: &testTimeout,
			},
		},
	},
	Route: &option.RouteOptions{
		AutoDetectInterface: true,
		Rules: []option.Rule{
			{
				Type: constant.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{"tun-in"},
					},
					RuleAction: option.RuleAction{
						Action: constant.RuleActionTypeSniff,
					},
				},
			},
			{
				Type: constant.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Protocol: badoption.Listable[string]{"dns"},
					},
					RuleAction: option.RuleAction{
						Action: constant.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: "dns-out",
						},
					},
				},
			},
			{
				Type: constant.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{"tun-in"},
						Domain:  badoption.Listable[string]{"google.com"},
					},
					RuleAction: option.RuleAction{
						Action: constant.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: "outline-out",
						},
					},
				},
			},
			{
				Type: constant.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Protocol: badoption.Listable[string]{"ssh"},
					},
					RuleAction: option.RuleAction{
						Action: constant.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: "direct",
						},
					},
				},
			},
			{
				Type: constant.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{"tun-in"},
					},
					RuleAction: option.RuleAction{
						Action: constant.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: "direct",
						},
					},
				},
			},
		},
	},
}
