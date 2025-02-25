package client

import (
	"net/netip"
	"os"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
)

func getopts() option.Options {
	var options = option.Options{
		Log: &option.LogOptions{
			Disabled: true,
		},
		DNS: &option.DNSOptions{
			Servers: []option.DNSServerOptions{
				{
					Tag:      "google",
					Address:  "8.8.8.8",
					Strategy: option.DomainStrategy(dns.DomainStrategyUseIPv4),
				},
			},
		},
		// 	Rules: rules,
		// 	DNSClientOptions: option.DNSClientOptions{
		// 		DisableCache: true,
		// 	},
		// },
		Inbounds: []option.Inbound{
			{
				Type: "tun",
				Tag:  "tun-in",
				Options: &option.TunInboundOptions{
					InterfaceName: "utun225",
					Address: badoption.Listable[netip.Prefix]{
						netip.MustParsePrefix("10.10.1.1/30"),
					},
					AutoRoute:   true,
					StrictRoute: false,
					// EndpointIndependentNat: true,
					Stack: "system",
				},
			},
		},
		Outbounds: []option.Outbound{
			{
				Type: "direct",
				Tag:  "direct",
			},
			{
				Type: "dns",
				Tag:  "dns-out",
			},
			{
				Type: "algeneva",
				Tag:  "algeneva-out",
			},
			{
				Type: "proxyless",
				Tag:  "proxyless-out",
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
							Inbound: badoption.Listable[string]{"tun-in"},
							Domain:  badoption.Listable[string]{"ipconfig.io"},
						},
						RuleAction: option.RuleAction{
							Action: constant.RuleActionTypeRoute,
							RouteOptions: option.RouteActionOptions{
								Outbound: "algeneva-out",
							},
						},
					},
				},
				{
					Type: constant.RuleTypeDefault,
					DefaultOptions: option.DefaultRule{
						RawDefaultRule: option.RawDefaultRule{
							Inbound: badoption.Listable[string]{"tun-in"},
							Domain:  badoption.Listable[string]{"ifconfig.me"},
						},
						RuleAction: option.RuleAction{
							Action: constant.RuleActionTypeRoute,
							RouteOptions: option.RouteActionOptions{
								Outbound: "proxyless-out",
							},
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
			},
		},
	}

	options.Log = &option.LogOptions{
		Disabled:     false,
		Level:        "trace",
		Output:       "debug.log",
		Timestamp:    true,
		DisableColor: true,
	}
	return options
}

var rules = []option.DNSRule{
	// not used currently
	{
		Type: "default",
		DefaultOptions: option.DefaultDNSRule{
			RawDefaultDNSRule: option.RawDefaultDNSRule{
				Inbound:                             []string{"tun-in"},
				IPVersion:                           4,
				QueryType:                           []option.DNSQueryType{},
				Network:                             []string{},
				AuthUser:                            []string{},
				Protocol:                            []string{},
				Domain:                              []string{"ipconfig.io"},
				DomainSuffix:                        []string{},
				DomainKeyword:                       []string{},
				DomainRegex:                         []string{},
				Geosite:                             []string{"us"},
				SourceGeoIP:                         []string{},
				GeoIP:                               []string{},
				IPCIDR:                              []string{},
				IPIsPrivate:                         false,
				SourceIPCIDR:                        []string{},
				SourceIPIsPrivate:                   false,
				SourcePort:                          []uint16{},
				SourcePortRange:                     []string{},
				Port:                                []uint16{},
				PortRange:                           []string{},
				ProcessName:                         []string{},
				ProcessPath:                         []string{},
				ProcessPathRegex:                    []string{},
				PackageName:                         []string{},
				User:                                []string{},
				UserID:                              []int32{},
				Outbound:                            []string{},
				ClashMode:                           "",
				NetworkType:                         []option.InterfaceType{},
				NetworkIsExpensive:                  false,
				NetworkIsConstrained:                false,
				WIFISSID:                            []string{},
				WIFIBSSID:                           []string{},
				RuleSet:                             []string{},
				RuleSetIPCIDRMatchSource:            false,
				RuleSetIPCIDRAcceptEmpty:            false,
				Invert:                              false,
				Deprecated_RulesetIPCIDRMatchSource: false,
			},
			DNSRuleAction: option.DNSRuleAction{
				Action: "route",
				RouteOptions: option.DNSRouteActionOptions{
					Server:       "local",
					DisableCache: false,
					RewriteTTL:   new(uint32),
					ClientSubnet: nil,
				},
				RouteOptionsOptions: option.DNSRouteOptionsActionOptions{},
				RejectOptions:       option.RejectActionOptions{},
			},
		},
		LogicalOptions: option.LogicalDNSRule{},
	},
}

func save(opts option.Options) {
	b, err := json.Marshal(opts)
	if err != nil {
		glog.Fatal(err)
	}
	os.WriteFile("client/opts.json", b, 0644)
}
