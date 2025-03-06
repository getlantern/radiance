package boxoptions

import (
	"net/netip"

	"github.com/sagernet/sing-box/constant"
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
						HTTPS: &option.HTTPSEntryConfig{
							Name: "dns.google",
						},
					},
					{
						HTTPS: &option.HTTPSEntryConfig{
							Name:    "cloudflare-dns.com.",
							Address: "cloudflare.net.",
						},
					},
					{
						HTTPS: &option.HTTPSEntryConfig{
							Name:    "doh.dns.sb",
							Address: "https://doh.dns.sb/dns-query",
						},
					},
					{
						System: &struct{}{},
					},
				},
				TLS:         []string{"", "split:1", "split:2,20*5", "split:200|disorder:1", "tlsfrag:1"},
				Domains:     []string{"api.iantem.io", "google.com"},
				TestTimeout: "10s",
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
						Domain:  badoption.Listable[string]{"api.iantem.io", "google.com"},
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
