package vpn

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/servers"
)

const (
	ServerGroupLantern = "lantern"
	ServerGroupUser    = "user"

	autoLantern = "auto-lantern"
	autoUser    = "auto-user"
	autoAll     = "auto-all"
)

func baseOpts() O.Options {
	return O.Options{
		Log: &O.LogOptions{
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
					Address: []netip.Prefix{
						netip.MustParsePrefix("10.10.1.1/30"),
					},
					AutoRoute:   true,
					StrictRoute: true,
				},
			},
		},
		Outbounds: []O.Outbound{
			{
				Type:    C.TypeDirect,
				Tag:     "direct",
				Options: &O.DirectOutboundOptions{},
			},
		},
		Route: &O.RouteOptions{
			AutoDetectInterface: true,
			// routing rules are evaluated in the order they are defined and the first matching rule
			// is applied. So order is important here.
			// DO NOT change the order of the first three rules or things will break. They MUST always
			// be the first three rules.
			Rules: []O.Rule{
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeSniff,
						},
					},
				},
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							Protocol: []string{"dns"},
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeHijackDNS,
						},
					},
				},
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultRule{
						RawDefaultRule: O.RawDefaultRule{
							RuleSet: []string{splitTunnelTag},
						},
						RuleAction: O.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: O.RouteActionOptions{
								Outbound: "direct",
							},
						},
					},
				},
				groupRule(autoAll),
				groupRule(autoLantern),
				groupRule(autoUser),
				groupRule(ServerGroupLantern),
				groupRule(ServerGroupUser),
			},
			RuleSet: []O.RuleSet{
				{
					Type: C.RuleSetTypeLocal,
					Tag:  splitTunnelTag,
					LocalOptions: O.LocalRuleSet{
						Path: splitTunnelFile,
					},
					Format: C.RuleSetFormatSource,
				},
			},
		},
		Experimental: &O.ExperimentalOptions{
			ClashAPI: &O.ClashAPIOptions{
				DefaultMode:        ServerGroupLantern,
				ModeList:           []string{ServerGroupLantern, ServerGroupUser, autoLantern, autoUser, autoAll},
				ExternalController: "", // intentionally left empty
			},
			CacheFile: &O.CacheFileOptions{
				Enabled: true,
				Path:    cacheFileName,
				CacheID: cacheID,
			},
		},
	}
}

// buildOptions builds the box options using the config options and user servers. URLTest outbounds
// will only be added if mode is set to [autoLantern], [autoUser], or [autoAll], and only for the
// respective group.
func buildOptions(mode, path string) (O.Options, error) {
	// load config options
	confPath := filepath.Join(path, app.ConfigFileName)
	content, err := os.ReadFile(confPath)
	if err != nil {
		return O.Options{}, fmt.Errorf("read config file: %w", err)
	}
	cOpts, err := unmarshalConfig(content)
	if err != nil {
		return O.Options{}, fmt.Errorf("unmarshal config: %w", err)
	}

	// load user servers
	mgr, err := servers.NewManager(path, nil)
	if err != nil {
		return O.Options{}, fmt.Errorf("create server manager: %w", err)
	}
	uOpts := mgr.Servers()

	// build options
	opts := baseOpts()

	// update default options and paths
	opts.Experimental.CacheFile.Path = filepath.Join(path, cacheFileName)
	opts.Experimental.ClashAPI.DefaultMode = mode
	splitTunnelFilePath := filepath.Join(path, splitTunnelFile)
	opts.Route.RuleSet[0].LocalOptions.Path = splitTunnelFilePath

	mergeOuts := func(dst, src *O.Options) []string {
		dst.Outbounds = append(dst.Outbounds, src.Outbounds...)
		dst.Endpoints = append(dst.Endpoints, src.Endpoints...)
		var tags []string
		for _, out := range src.Outbounds {
			tags = append(tags, out.Tag)
		}
		for _, ep := range src.Endpoints {
			tags = append(tags, ep.Tag)
		}
		return tags
	}

	// merge config options into base
	ltnTags := mergeOuts(&opts, &cOpts)
	opts.Outbounds = append(opts.Outbounds, selectorOutbound(ServerGroupLantern, ltnTags))
	opts.Route.Rules = append(opts.Route.Rules, cOpts.Route.Rules...)
	opts.Route.RuleSet = append(opts.Route.RuleSet, cOpts.Route.RuleSet...)
	opts.DNS.Servers = append(opts.DNS.Servers, cOpts.DNS.Servers...)

	switch plat := app.Platform; plat {
	case "android":
		opts.Route.OverrideAndroidVPN = true
	case "linux":
		opts.Inbounds[0].Options.(*O.TunInboundOptions).AutoRedirect = true
	}

	// merge user servers into base
	userTags := mergeOuts(&opts, &O.Options{Outbounds: uOpts.Outbounds, Endpoints: uOpts.Endpoints})
	opts.Outbounds = append(opts.Outbounds, selectorOutbound(ServerGroupUser, userTags))

	// if auto mode is selected, add URLTest outbounds for respective group
	switch mode {
	case autoAll:
		tags := append(ltnTags, userTags...)
		opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoAll, tags))
	case autoLantern:
		opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoLantern, ltnTags))
	case autoUser:
		opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoUser, userTags))
	}
	return opts, nil
}

// helper functions

func urlTestOutbound(group string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: C.TypeURLTest,
		Tag:  group,
		Options: &O.URLTestOutboundOptions{
			Outbounds: outbounds,
		},
	}
}

func selectorOutbound(group string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: C.TypeSelector,
		Tag:  group,
		Options: &O.SelectorOutboundOptions{
			Outbounds:                 outbounds,
			InterruptExistConnections: false,
		},
	}
}

func groupRule(group string) O.Rule {
	return O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{
				Inbound:   []string{"tun-in"},
				ClashMode: group,
			},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeRoute,
				RouteOptions: O.RouteActionOptions{
					Outbound: group,
				},
			},
		},
	}
}
