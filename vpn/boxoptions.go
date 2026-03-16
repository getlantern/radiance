package vpn

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"time"

	lcommon "github.com/getlantern/common"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	box "github.com/getlantern/lantern-box"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/kindling"
	"github.com/getlantern/radiance/servers"
)

const (
	autoAllTag = "auto"

	autoLanternTag = "auto-lantern"
	autoUserTag    = "auto-user"

	urlTestInterval    = 3 * time.Minute // must be less than urlTestIdleTimeout
	urlTestIdleTimeout = 15 * time.Minute

	cacheID       = "lantern"
	cacheFileName = "lantern.cache"
)

// this is the base options that is need for everything to work correctly. this should not be
// changed unless you know what you're doing.
func baseOpts(basePath string) O.Options {
	splitTunnelPath := filepath.Join(basePath, splitTunnelFile)

	loopbackAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	return O.Options{
		Log: &O.LogOptions{
			Level:        "debug",
			Output:       "lantern-box.log",
			Timestamp:    true,
			DisableColor: true,
		},
		DNS: &O.DNSOptions{
			RawDNSOptions: O.RawDNSOptions{
				Servers: buildDNSServers(),
				Rules:   buildDNSRules(),
				// Fallback DNS when no rules match.
				Final: "dns_local",
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
					MTU:         1500,
				},
			},
			{
				Type: C.TypeMixed,
				Tag:  bypass.BypassInboundTag,
				Options: &O.HTTPMixedInboundOptions{
					ListenOptions: O.ListenOptions{
						Listen:     &loopbackAddr,
						ListenPort: bypass.ProxyPort,
					},
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
			{
				Type: C.TypeHTTP,
				Tag:  "kindling-proxy",
				Options: &O.HTTPOutboundOptions{
					ServerOptions: O.ServerOptions{
						Server:     "127.0.0.1",
						ServerPort: kindling.ProxyPort,
					},
				},
			},
		},
		Route: &O.RouteOptions{
			AutoDetectInterface: true,
			Rules:               baseRoutingRules(),
			RuleSet: []O.RuleSet{
				{
					Type: C.RuleSetTypeLocal,
					Tag:  splitTunnelTag,
					LocalOptions: O.LocalRuleSet{
						Path: splitTunnelPath,
					},
					Format: C.RuleSetFormatSource,
				},
			},
		},
		Experimental: &O.ExperimentalOptions{
			ClashAPI: &O.ClashAPIOptions{
				DefaultMode:        autoAllTag,
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

func baseRoutingRules() []O.Rule {
	// routing rules are evaluated in the order they are defined and the first matching rule
	// is applied. So order is important here.
	// The rules MUST be in this order to ensure proper functionality:
	// 1.    Enable traffic sniffing
	// 2.    Hijack DNS to allow sing-box to handle DNS requests
	// 3.    Route bypass proxy traffic directly (for kindling connections)
	// 4.    Route private IPs to direct outbound
	// 5.    Split tunnel rule (user-configurable)
	// 6.    Rules from config file (added in buildOptions)
	// 7-9.  Group rules for auto, lantern, and user (added in buildOptions)
	// 10.   Catch-all blocking rule (added in buildOptions). This ensures that any traffic not covered
	//       by previous rules does not automatically bypass the VPN.
	//
	// * DO NOT change the order of these rules unless you know what you're doing. Changing these
	//   rules or their order can break certain functionalities like DNS resolution, smart connect,
	//   or split tunneling.
	//
	// The default rule type uses the following matching logic:
	// (domain || domain_suffix || domain_keyword || domain_regex || geosite || geoip || ip_cidr || ip_is_private) &&
	// (port || port_range) &&
	// (source_geoip || source_ip_cidr || source_ip_is_private) &&
	// (source_port || source_port_range) &&
	// other fields
	//
	// rule-sets are merged into the appropriate fields before evaluation instead of being evaluated separately.
	rules := []O.Rule{
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
		{ // Route bypass proxy traffic directly (for kindling connections)
			Type: C.RuleTypeDefault,
			DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{
					Inbound: []string{bypass.BypassInboundTag},
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
					IPIsPrivate: true,
				},
				RuleAction: O.RuleAction{
					Action: C.RuleActionTypeRoute,
					RouteOptions: O.RouteActionOptions{
						Outbound: "direct",
					},
				},
			},
		},
		{ // split tunnel rule
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
	}
	return rules
}

// buildOptions builds the box options using the config options and user servers.
func buildOptions(ctx context.Context, path string) (O.Options, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "buildOptions")
	defer span.End()

	slog.Log(nil, internal.LevelTrace, "Starting buildOptions", "path", path)

	opts := baseOpts(path)
	slog.Debug("Base options initialized")

	// update default options and paths
	opts.Experimental.CacheFile.Path = filepath.Join(path, cacheFileName)

	slog.Log(nil, internal.LevelTrace, "Updated default options and paths",
		"cacheFilePath", opts.Experimental.CacheFile.Path,
		"clashAPIDefaultMode", opts.Experimental.ClashAPI.DefaultMode,
	)

	if _, useSocks := env.Get[bool](env.UseSocks); useSocks {
		socksAddr, _ := env.Get[string](env.SocksAddress)
		slog.Info("Using SOCKS proxy for inbound as per environment variable", "socksAddr", socksAddr)
		addrPort, err := netip.ParseAddrPort(socksAddr)
		if err != nil {
			return O.Options{}, fmt.Errorf("invalid SOCKS address: %w", err)
		}
		addr := badoption.Addr(addrPort.Addr())
		socksIn := O.Inbound{
			Type: C.TypeMixed,
			Tag:  "http-socks-in",
			Options: &O.HTTPMixedInboundOptions{
				ListenOptions: O.ListenOptions{
					Listen:     &addr,
					ListenPort: addrPort.Port(),
				},
			},
		}
		opts.Inbounds = []O.Inbound{socksIn}
	} else {
		// platform-specific overrides
		switch common.Platform {
		case "android":
			opts.Route.OverrideAndroidVPN = true
			slog.Debug("Android platform detected, OverrideAndroidVPN set to true")
		case "linux":
			opts.Inbounds[0].Options.(*O.TunInboundOptions).AutoRedirect = true
			slog.Debug("Linux platform detected, AutoRedirect set to true")
		}
	}

	// Load config file
	confPath := filepath.Join(path, common.ConfigFileName)
	slog.Debug("Loading config file", "confPath", confPath)
	cfg, err := loadConfig(confPath)
	if err != nil {
		slog.Error("Failed to load config options", "error", err)
		return O.Options{}, err
	}

	// add smart routing and ad block rules
	if settings.GetBool(settings.SmartRoutingKey) && len(cfg.SmartRouting) > 0 {
		slog.Debug("Adding smart-routing rules")
		outbounds, rules, rulesets := cfg.SmartRouting.ToOptions(urlTestInterval, urlTestIdleTimeout)
		opts.Outbounds = append(opts.Outbounds, outbounds...)
		opts.Route.Rules = append(opts.Route.Rules, rules...)
		opts.Route.RuleSet = append(opts.Route.RuleSet, rulesets...)
	}
	if settings.GetBool(settings.AdBlockKey) && len(cfg.AdBlock) > 0 {
		slog.Debug("Adding ad-block rules")
		rule, rulesets := cfg.AdBlock.ToOptions()
		opts.Route.Rules = append(opts.Route.Rules, rule)
		opts.Route.RuleSet = append(opts.Route.RuleSet, rulesets...)
	}

	var lanternTags []string
	configOpts := cfg.Options
	if len(configOpts.Outbounds) == 0 && len(configOpts.Endpoints) == 0 {
		slog.Warn("Config loaded but no outbounds or endpoints found")
	}
	lanternTags = mergeAndCollectTags(&opts, &configOpts)
	slog.Debug("Merged config options", "tags", lanternTags)

	appendGroupOutbounds(&opts, servers.SGLantern, autoLanternTag, lanternTags)

	// Load user servers
	slog.Debug("Loading user servers")
	userOpts, err := loadUserOptions(path)
	if err != nil {
		slog.Error("Failed to load user servers", "error", err)
		return O.Options{}, err
	}
	var userTags []string
	if len(userOpts.Outbounds) == 0 && len(userOpts.Endpoints) == 0 {
		slog.Info("No user servers found")
	} else {
		userTags = mergeAndCollectTags(&opts, &userOpts)
		slog.Debug("Merged user server options", "tags", userTags)
	}
	appendGroupOutbounds(&opts, servers.SGUser, autoUserTag, userTags)

	if len(lanternTags) == 0 && len(userTags) == 0 {
		return O.Options{}, errors.New("no outbounds or endpoints found in config or user servers")
	}

	// Add auto all outbound
	opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoAllTag, []string{autoLanternTag, autoUserTag}))

	// Add routing rules for the groups
	opts.Route.Rules = append(opts.Route.Rules, groupRule(autoAllTag))
	opts.Route.Rules = append(opts.Route.Rules, groupRule(servers.SGLantern))
	opts.Route.Rules = append(opts.Route.Rules, groupRule(servers.SGUser))

	// catch-all rule to ensure no fallthrough
	opts.Route.Rules = append(opts.Route.Rules, catchAllBlockerRule())
	slog.Debug("Finished building options", slog.String("env", common.Env()))

	span.AddEvent("finished building options", trace.WithAttributes(
		attribute.String("options", string(writeBoxOptions(path, opts))),
		attribute.String("env", common.Env()),
	))
	return opts, nil
}

const debugLanternBoxOptionsFilename = "debug-lantern-box-options.json"

// writeBoxOptions marshals the options as JSON and stores them in a file so we can debug them
// we can ignore the errors here since the tunnel will error out anyway if something is wrong
func writeBoxOptions(path string, opts O.Options) []byte {
	buf, err := json.MarshalContext(box.BaseContext(), opts)
	if err != nil {
		slog.Warn("failed to marshal options while writing debug box options", slog.Any("error", err))
		return nil
	}

	var b bytes.Buffer
	if err := stdjson.Indent(&b, buf, "", "  "); err != nil {
		slog.Warn("failed to indent marshaled options while writing debug box options", slog.Any("error", err))
		return buf
	}
	if err := atomicfile.WriteFile(filepath.Join(path, debugLanternBoxOptionsFilename), b.Bytes(), 0644); err != nil {
		slog.Warn("failed to write options file", slog.Any("error", err))
		return buf
	}
	return b.Bytes()
}

///////////////////////
// Helper functions //
//////////////////////

func loadConfig(path string) (lcommon.ConfigResponse, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return lcommon.ConfigResponse{}, fmt.Errorf("load config: %w", err)
	}
	if cfg == nil {
		return lcommon.ConfigResponse{}, nil
	}
	return cfg.ConfigResponse, nil
}

func loadUserOptions(path string) (O.Options, error) {
	mgr, err := servers.NewManager(path)
	if err != nil {
		return O.Options{}, fmt.Errorf("server manager: %w", err)
	}
	u := mgr.Servers()[servers.SGUser]
	return O.Options{Outbounds: u.Outbounds, Endpoints: u.Endpoints}, nil
}

// mergeAndCollectTags merges src into dst and returns all outbound/endpoint tags from src.
func mergeAndCollectTags(dst, src *O.Options) []string {
	dst.Outbounds = append(dst.Outbounds, src.Outbounds...)
	dst.Endpoints = append(dst.Endpoints, src.Endpoints...)

	if src.Route != nil {
		dst.Route.Rules = append(dst.Route.Rules, src.Route.Rules...)
		dst.Route.RuleSet = append(dst.Route.RuleSet, src.Route.RuleSet...)
	}
	// overwrite base DNS options with config from src (server)
	if src.DNS != nil {
		dns := *src.DNS
		dst.DNS = &dns
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

func useIfNotZero[T comparable](newVal, oldVal T) T {
	var zero T
	if newVal != zero {
		return newVal
	}
	return oldVal
}

func appendGroupOutbounds(opts *O.Options, serverGroup, autoTag string, tags []string) {
	opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoTag, tags))
	opts.Outbounds = append(opts.Outbounds, selectorOutbound(serverGroup, append([]string{autoTag}, tags...)))
	slog.Log(
		nil, internal.LevelTrace, "Added group outbounds",
		"serverGroup", serverGroup,
		"tags", tags,
		"outbounds", opts.Outbounds[len(opts.Outbounds)-2:],
	)
}

func groupAutoTag(group string) string {
	switch group {
	case servers.SGLantern:
		return autoLanternTag
	case servers.SGUser:
		return autoUserTag
	case "all", "":
		return autoAllTag
	default:
		return ""
	}
}

func urlTestOutbound(tag string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: lbC.TypeMutableURLTest,
		Tag:  tag,
		Options: &lbO.MutableURLTestOutboundOptions{
			Outbounds:   outbounds,
			URL:         "https://google.com/generate_204",
			Interval:    badoption.Duration(urlTestInterval),
			IdleTimeout: badoption.Duration(urlTestIdleTimeout),
		},
	}
}

func selectorOutbound(group string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: lbC.TypeMutableSelector,
		Tag:  group,
		Options: &lbO.MutableSelectorOutboundOptions{
			Outbounds: outbounds,
		},
	}
}

func groupRule(group string) O.Rule {
	return O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{
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

func catchAllBlockerRule() O.Rule {
	return O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeReject,
			},
		},
	}
}

func newDNSServerOptions(typ, tag, server, domainResolver string) O.DNSServerOptions {
	var serverOpts any
	remoteOpts := O.RemoteDNSServerOptions{
		DNSServerAddressOptions: O.DNSServerAddressOptions{
			Server: server,
		},
	}
	if domainResolver != "" {
		remoteOpts.LocalDNSServerOptions = O.LocalDNSServerOptions{
			DialerOptions: O.DialerOptions{
				DomainResolver: &O.DomainResolveOptions{
					Server: domainResolver,
				},
			},
		}
	}
	switch typ {
	case C.DNSTypeTCP, C.DNSTypeUDP:
		serverOpts = remoteOpts
	case C.DNSTypeTLS:
		serverOpts = &O.RemoteTLSDNSServerOptions{
			RemoteDNSServerOptions: remoteOpts,
		}
	case C.DNSTypeHTTPS:
		serverOpts = &O.RemoteHTTPSDNSServerOptions{
			RemoteTLSDNSServerOptions: O.RemoteTLSDNSServerOptions{
				RemoteDNSServerOptions: remoteOpts,
			},
		}
	default:
		serverOpts = &O.LocalDNSServerOptions{}
	}

	return O.DNSServerOptions{
		Tag:     tag,
		Type:    typ,
		Options: serverOpts,
	}
}
