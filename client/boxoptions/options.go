package boxoptions

import (
	"net/netip"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json/badoption"

	exC "github.com/getlantern/sing-box-extensions/constant"

	"github.com/getlantern/radiance/option"
)

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
					AutoRoute:              true,
					StrictRoute:            true,
					EndpointIndependentNat: false,
					Stack:                  "system",
				},
			},
		},
		Outbounds: []O.Outbound{
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
				Type: exC.TypeOutline,
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
							Domain:  badoption.Listable[string]{"api.iantem.io", "google.com"},
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: O.RouteActionOptions{
								Outbound: "outline-out",
							},
						},
					},
				},
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							Protocol: badoption.Listable[string]{"ssh"},
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: O.RouteActionOptions{
								Outbound: "direct",
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
								Outbound: "direct",
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
