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
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	lcommon "github.com/getlantern/common"
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
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
)

const (
	AutoSelectTag   = "auto"
	ManualSelectTag = "manual"

	urlTestInterval    = 3 * time.Minute // must be less than urlTestIdleTimeout
	urlTestIdleTimeout = 15 * time.Minute

	cacheID       = "lantern"
	cacheFileName = "lantern.cache"
	// minAndroidSystemStackKernel is the minimum Linux kernel version (major.minor) required
	// for the system TUN stack to work reliably on Android only. Devices running a
	// kernel below this version fall back to gvisor. This constant has no effect on
	// other platforms.
	minAndroidSystemStackKernel = "5.10"
)

var reservedTags = []string{AutoSelectTag, ManualSelectTag, "direct", "block"}

func ReservedTags() []string {
	return slices.Clone(reservedTags)
}

type BoxOptions struct {
	BasePath string `json:"base_path,omitempty"`
	// Options contains the main options that are merged into the base options with the exception of
	// DNS, which overrides the base DNS options entirely instead of being merged. Options should
	// contain all servers (both lantern and user).
	Options O.Options `json:"options"`
	// SmartRouting contains smart routing rules to merge into the final options.
	SmartRouting lcommon.SmartRoutingRules `json:"smart_routing,omitempty"`
	// AdBlock contains ad block rules to merge into the final options.
	AdBlock lcommon.AdBlockRules `json:"ad_block,omitempty"`
	// BanditURLOverrides maps outbound tags to per-proxy callback URLs for
	// the bandit Thompson sampling system. When set, these override the
	// default MutableURLTest URL for each specific outbound, allowing the
	// server to detect which proxies successfully connected.
	BanditURLOverrides  map[string]string `json:"bandit_url_overrides,omitempty"`
	BanditThroughputURL string            `json:"bandit_throughput_url,omitempty"`
}

// this is the base options that is need for everything to work correctly. this should not be
// changed unless you know what you're doing.
func baseOpts(basePath string) O.Options {
	splitTunnelPath := filepath.Join(basePath, splitTunnelFile)
	cacheFile := filepath.Join(basePath, cacheFileName)
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
				DefaultMode:        AutoSelectTag,
				ModeList:           []string{ManualSelectTag, AutoSelectTag},
				ExternalController: "", // intentionally left empty
			},
			CacheFile: &O.CacheFileOptions{
				Enabled: true,
				Path:    cacheFile,
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
func buildOptions(bOptions BoxOptions) (O.Options, error) {
	_, span := otel.Tracer(tracerName).Start(context.Background(), "buildOptions")
	defer span.End()

	if len(bOptions.Options.Outbounds) == 0 && len(bOptions.Options.Endpoints) == 0 {
		return O.Options{}, errors.New("no outbounds or endpoints found in config or user servers")
	}

	slog.Log(nil, log.LevelTrace, "Starting buildOptions", "path", bOptions.BasePath)

	opts := baseOpts(bOptions.BasePath)
	slog.Debug("Base options initialized")

	if env.GetBool(env.UseSocks) {
		socksAddr, _ := env.Get(env.SocksAddress)
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
		switch common.Platform {
		case "android":
			opts.Route.OverrideAndroidVPN = true
			opts.Inbounds[0].Options.(*O.TunInboundOptions).Stack = "gvisor"
			slog.Debug("Android platform detected, OverrideAndroidVPN enabled and using gvisor tun stack")
		case "linux":
			opts.Inbounds[0].Options.(*O.TunInboundOptions).AutoRedirect = true
			slog.Debug("Linux platform detected, AutoRedirect enabled")
		}
	}

	// add smart routing and ad block rules
	smartRoutingRules := normalizeSmartRoutingRules(bOptions.SmartRouting)
	if len(smartRoutingRules) > 0 {
		slog.Info("Adding smart-routing rules")
		outbounds, rules, rulesets := smartRoutingRules.ToOptions(urlTestInterval, urlTestIdleTimeout)
		opts.Outbounds = append(opts.Outbounds, outbounds...)
		opts.Route.Rules = append(opts.Route.Rules, rules...)
		opts.Route.RuleSet = append(opts.Route.RuleSet, rulesets...)
	} else if len(bOptions.SmartRouting) > 0 && len(smartRoutingRules) == 0 {
		slog.Warn("No valid smart-routing rules found after normalization, skipping smart-routing configuration")
	}
	adBlockRules := normalizeAdBlockRules(bOptions.AdBlock)
	if len(adBlockRules) > 0 {
		slog.Info("Adding ad-block rules")
		rule, rulesets := bOptions.AdBlock.ToOptions()
		opts.Route.Rules = append(opts.Route.Rules, rule)
		opts.Route.RuleSet = append(opts.Route.RuleSet, rulesets...)
	} else if len(bOptions.AdBlock) > 0 && len(adBlockRules) == 0 {
		slog.Warn("No valid ad-block rules found after normalization, skipping ad-block configuration")
	}

	tags := mergeAndCollectTags(&opts, &bOptions.Options)

	// add mode selector outbounds and rules
	opts.Outbounds = append(opts.Outbounds, urlTestOutbound(AutoSelectTag, tags, bOptions.BanditURLOverrides))
	opts.Outbounds = append(opts.Outbounds, selectorOutbound(ManualSelectTag, tags))
	opts.Route.Rules = append(opts.Route.Rules, selectModeRule(AutoSelectTag))
	opts.Route.Rules = append(opts.Route.Rules, selectModeRule(ManualSelectTag))

	// catch-all rule to ensure no fallthrough
	opts.Route.Rules = append(opts.Route.Rules, catchAllBlockerRule())
	slog.Debug("Finished building options", "env", common.Env())

	span.AddEvent("finished building options", trace.WithAttributes(
		attribute.String("options", string(writeBoxOptions(bOptions.BasePath, opts))),
	))
	return opts, nil
}

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
	if err := atomicfile.WriteFile(filepath.Join(path, internal.DebugBoxOptionsFileName), b.Bytes(), 0644); err != nil {
		slog.Warn("failed to write options file", slog.Any("error", err))
		return buf
	}
	return b.Bytes()
}

//////////////////////
// Helper functions //
//////////////////////

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

func normalizeSmartRoutingRules(rules lcommon.SmartRoutingRules) lcommon.SmartRoutingRules {
	normalized := make(lcommon.SmartRoutingRules, 0, len(rules))
	for _, sr := range rules {
		cleaned := make([]string, 0, len(sr.Outbounds))
		for _, outbound := range sr.Outbounds {
			tag := strings.TrimSpace(outbound)
			if tag == "" {
				continue
			}
			cleaned = append(cleaned, tag)
		}

		sr.Outbounds = cleaned
		if len(sr.Outbounds) == 0 {
			slog.Warn("Skipping smart-routing rule with no outbounds", "category", sr.Category)
			continue
		}

		normalized = append(normalized, sr)
	}
	return normalized
}

func normalizeAdBlockRules(rules lcommon.AdBlockRules) lcommon.AdBlockRules {
	normalized := make(lcommon.AdBlockRules, 0, len(rules))
	for _, rule := range rules {
		tag := strings.TrimSpace(rule.Tag)
		if tag == "" {
			slog.Warn("Skipping ad-block rule with empty tag")
			continue
		}
		rule.Tag = tag
		normalized = append(normalized, rule)
	}
	return normalized
}

func urlTestOutbound(tag string, outbounds []string, urlOverrides map[string]string) O.Outbound {
	return O.Outbound{
		Type: lbC.TypeMutableURLTest,
		Tag:  tag,
		Options: &lbO.MutableURLTestOutboundOptions{
			Outbounds:    outbounds,
			URL:          "https://google.com/generate_204",
			URLOverrides: urlOverrides,
			Interval:     badoption.Duration(urlTestInterval),
			IdleTimeout:  badoption.Duration(urlTestIdleTimeout),
		},
	}
}

func selectorOutbound(tag string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: lbC.TypeMutableSelector,
		Tag:  tag,
		Options: &lbO.MutableSelectorOutboundOptions{
			Outbounds: outbounds,
		},
	}
}

func selectModeRule(mode string) O.Rule {
	return O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{
				ClashMode: mode,
			},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeRoute,
				RouteOptions: O.RouteActionOptions{
					Outbound: mode,
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
