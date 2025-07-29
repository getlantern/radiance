package vpn

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"

	LC "github.com/getlantern/common"
	sbx "github.com/getlantern/sing-box-extensions"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"
)

const (
	autoAllTag = "auto-all"

	autoLanternTag = "auto-lantern"
	autoUserTag    = "auto-user"

	urlTestInterval    = 3 * time.Minute // must be less than urlTestIdleTimeout
	urlTestIdleTimeout = 15 * time.Minute

	cacheID       = "lantern"
	cacheFileName = "lantern.cache"
)

// this is the base options that is need for everything to work correctly. this should not be
// changed unless you know what you're doing.
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
			{
				Type:    C.TypeBlock,
				Tag:     "block",
				Options: &O.StubOptions{},
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
				groupRule(autoAllTag),
				groupRule(servers.SGLantern),
				groupRule(servers.SGUser),
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
				DefaultMode:        servers.SGLantern,
				ModeList:           []string{servers.SGLantern, servers.SGUser, autoAllTag},
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
func buildOptions(group, path string) (O.Options, error) {
	// build options
	slog.Log(nil, internal.LevelTrace, "Starting buildOptions", "group", group, "path", path)
	opts := baseOpts()
	slog.Debug("Base options initialized")

	// update default options and paths
	opts.Experimental.CacheFile.Path = filepath.Join(path, cacheFileName)
	opts.Experimental.ClashAPI.DefaultMode = group
	splitTunnelFilePath := filepath.Join(path, splitTunnelFile)
	opts.Route.RuleSet[0].LocalOptions.Path = splitTunnelFilePath
	slog.Log(nil, internal.LevelTrace, "Updated default options and paths",
		"cacheFilePath", opts.Experimental.CacheFile.Path,
		"splitTunnelFilePath", splitTunnelFilePath,
	)

	switch plat := common.Platform; plat {
	case "android":
		opts.Route.OverrideAndroidVPN = true
		slog.Debug("Android platform detected, OverrideAndroidVPN set to true")
	case "linux":
		opts.Inbounds[0].Options.(*O.TunInboundOptions).AutoRedirect = true
		slog.Debug("Linux platform detected, AutoRedirect set to true")
	}

	// load and merge config options into base
	confPath := filepath.Join(path, common.ConfigFileName)
	slog.Debug("Loading config file", "confPath", confPath)
	content, err := os.ReadFile(confPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("Failed to read config file", "error", err)
		return O.Options{}, fmt.Errorf("read config file: %w", err)
	}
	var ltnTags []string
	if len(content) > 0 {
		slog.Log(nil, internal.LevelTrace, "Config file found, unmarshalling")
		cfg, err := json.UnmarshalExtendedContext[LC.ConfigResponse](sbx.BoxContext(), content)
		if err != nil {
			slog.Error("Failed to unmarshal config", "error", err)
			return O.Options{}, fmt.Errorf("unmarshal config: %w", err)
		}
		cOpts := cfg.Options

		ltnTags = mergeOpts(&opts, &cOpts)
		slog.Debug("Merged lantern config options", "tags", ltnTags)
		opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoLanternTag, ltnTags))
		opts.Outbounds = append(
			opts.Outbounds,
			selectorOutbound(servers.SGLantern, append([]string{autoLanternTag}, ltnTags...)),
		)
		slog.Log(nil, internal.LevelTrace, "Added lantern outbounds", "outbounds", opts.Outbounds)
	} else {
		slog.Info("No config file found")
		slog.Debug("Using placeholder outbound")
		opts.Outbounds = append(opts.Outbounds, selectorOutbound(servers.SGLantern, []string{"block"}))
	}

	// load and merge user servers into base
	slog.Debug("Loading user servers")
	mgr, err := servers.NewManager(path)
	if err != nil {
		slog.Error("Failed to create server manager", "error", err)
		return O.Options{}, fmt.Errorf("server manager: %w", err)
	}
	uOpts := mgr.Servers()[servers.SGUser]

	var userTags []string
	if len(uOpts.Outbounds)+len(uOpts.Endpoints) > 0 {
		userTags = mergeOpts(&opts, &O.Options{Outbounds: uOpts.Outbounds, Endpoints: uOpts.Endpoints})
		slog.Debug("Merged user server options", "userTags", userTags)
		opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoUserTag, userTags))
		opts.Outbounds = append(
			opts.Outbounds,
			selectorOutbound(servers.SGUser, append([]string{autoUserTag}, userTags...)),
		)
		slog.Log(nil, internal.LevelTrace, "Added user outbounds", "outbounds", opts.Outbounds)
	} else {
		slog.Info("No user servers found")
		slog.Debug("Using placeholder outbound")
		opts.Outbounds = append(opts.Outbounds, selectorOutbound(servers.SGUser, []string{"block"}))
	}

	allTags := slices.Concat(ltnTags, userTags)
	if len(allTags) == 0 {
		slog.Warn("No tags found")
		slog.Debug("Using placeholder outbound")
		allTags = []string{"block"}
	}
	opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoAllTag, allTags))
	slog.Debug("Final outbounds set", "outbounds", opts.Outbounds)
	slog.Log(nil, internal.LevelTrace, "buildOptions completed successfully")
	return opts, nil
}

// helper functions

func mergeOpts(dst, src *O.Options) []string {
	dst.Outbounds = append(dst.Outbounds, src.Outbounds...)
	dst.Endpoints = append(dst.Endpoints, src.Endpoints...)

	if src.Route != nil {
		dst.Route.Rules = append(dst.Route.Rules, src.Route.Rules...)
		dst.Route.RuleSet = append(dst.Route.RuleSet, src.Route.RuleSet...)
	}
	if src.DNS != nil {
		dst.DNS.Servers = append(dst.DNS.Servers, src.DNS.Servers...)
	}

	var tags []string
	for _, out := range src.Outbounds {
		tags = append(tags, out.Tag)
	}
	for _, ep := range src.Endpoints {
		tags = append(tags, ep.Tag)
	}
	return tags
}

func groupAutoTag(group string) string {
	switch group {
	case servers.SGLantern:
		return autoLanternTag
	case servers.SGUser:
		return autoUserTag
	default:
		return autoAllTag
	}
}

func urlTestOutbound(tag string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: C.TypeURLTest,
		Tag:  tag,
		Options: &O.URLTestOutboundOptions{
			Outbounds:   outbounds,
			Interval:    badoption.Duration(urlTestInterval),
			IdleTimeout: badoption.Duration(urlTestIdleTimeout),
		},
	}
}

func selectorOutbound(group string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: C.TypeSelector,
		Tag:  group,
		Options: &O.SelectorOutboundOptions{
			Outbounds: outbounds,
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
