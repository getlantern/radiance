package vpn

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	lcommon "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/fileperm"
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
	// InitialServer chooses the outbound selected when the tunnel starts.
	// Empty or AutoSelectTag puts the tunnel in auto mode; any other tag
	// must match an outbound or endpoint and forces manual selection.
	InitialServer string `json:"initial_server,omitempty"`
	// BanditURLOverrides maps outbound tags to per-proxy callback URLs for
	// the bandit Thompson sampling system. When set, these override the
	// default MutableURLTest URL for each specific outbound, allowing the
	// server to detect which proxies successfully connected.
	BanditURLOverrides  map[string]string `json:"bandit_url_overrides,omitempty"`
	BanditThroughputURL string            `json:"bandit_throughput_url,omitempty"`
	// URLTestSeed seeds the tunnel's URL test history storage at startup so
	// prior latency results survive across tunnel close/open. Keyed by
	// outbound/endpoint tag.
	URLTestSeed map[string]adapter.URLTestHistory `json:"-"`
}

// isGlobalIPv6 reports whether ip is a globally-routable unicast IPv6
// address (RFC 4291 2000::/3). Returns false for IPv4 (including v4-mapped
// v6), link-local, ULA (fc00::/7), loopback, unspecified, multicast, and
// the unassigned/reserved v6 ranges.
//
// We don't use net.IP.IsGlobalUnicast() because Go's stdlib also returns
// true for ULA — an interface with only ULA addresses (e.g. Tailscale or
// a corporate v6 VPN) doesn't indicate real v6 connectivity to the public
// internet, and that's the signal we care about for the TUN's v6 routing.
func isGlobalIPv6(ip net.IP) bool {
	if ip.To4() != nil {
		return false
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return false
	}
	// 2000::/3 — first three bits are 001; in the first byte that's
	// the high three bits matching 0x20 with mask 0xe0.
	return ip16[0]&0xe0 == 0x20
}

// hasGlobalIPv6 returns true if the system has at least one global unicast
// IPv6 address on a non-loopback interface. Used to decide whether to
// install an IPv6 ULA on the TUN inbound.
//
// We need the v6 ULA on dual-stack networks so sing-box's auto_route can
// install a v6 default route through the TUN — otherwise v6 traffic from
// IPv6-preferring apps (everything Chrome talks to Google with) leaks past
// the VPN. But adding the v6 ULA on a v4-only network has been observed to
// break some configurations in ways we have not narrowed down. Detecting
// presence of a real global v6 address before adding the ULA gates the
// behavior to the case where it's needed.
//
// Pure local syscall; runs in microseconds. Not cached; called once per
// tunnel start so a roaming user picks up network changes on reconnect.
func hasGlobalIPv6() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if isGlobalIPv6(ipnet.IP) {
				return true
			}
		}
	}
	return false
}

// baseOpts returns the minimum sing-box options required for the tunnel to
// function. Do not modify without understanding the downstream effects.
func baseOpts(basePath string) O.Options {
	splitTunnelPath := filepath.Join(basePath, splitTunnelFile)
	cacheFile := filepath.Join(basePath, cacheFileName)
	loopbackAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))

	// IPv6 on the TUN is conditional. See hasGlobalIPv6 doc for rationale.
	var tunInet6 []netip.Prefix
	if hasGlobalIPv6() {
		tunInet6 = []netip.Prefix{netip.MustParsePrefix("fdfe:dcba:9876::1/126")}
		slog.Info("vpn: TUN configured with IPv6 ULA (system has global v6)")
	} else {
		slog.Info("vpn: TUN configured IPv4-only (system has no global v6)")
	}
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
					// IPv6 ULA on the TUN — conditionally enabled when the
					// system has a real global v6 address; see hasGlobalIPv6.
					Inet6Address:           tunInet6,
					AutoRoute:              true,
					StrictRoute:            true,
					EndpointIndependentNat: true, // needed for QUIC migration and hole-punching
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
				Enabled:     true,
				Path:        cacheFile,
				CacheID:     cacheID,
				StoreFakeIP: true,
				StoreRDRC:   true,
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
	// 7,8.  Group rules for auto and manual selector modes (added in buildOptions).
	// 9.    Catch-all blocking rule (added in buildOptions). This ensures that any traffic not covered
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
			kv := kernelVersion()
			slog.Debug("detected kernel version", "kernel", kv)
			if kv == "" {
				slog.Warn("kernel version unknown, keeping default TUN stack")
			} else if kernelBelow(kv, minAndroidSystemStackKernel) {
				opts.Inbounds[0].Options.(*O.TunInboundOptions).Stack = "gvisor"
				slog.Info("kernel below 5.10, using gvisor TUN stack", "kernel", kv)
			}
			slog.Debug("Android platform detected, OverrideAndroidVPN set to true")
		case "linux":
			opts.Inbounds[0].Options.(*O.TunInboundOptions).AutoRedirect = true
			slog.Debug("Linux platform detected, AutoRedirect set to true")
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
	initial := bOptions.InitialServer
	if initial == "" || initial == AutoSelectTag {
		opts.Experimental.ClashAPI.DefaultMode = AutoSelectTag
	} else {
		// The manual selector defaults to its first tag, so place initial at index 0.
		i := slices.Index(tags, initial)
		if i == -1 {
			return O.Options{}, fmt.Errorf("initial server tag %q not found in outbounds or endpoints", initial)
		}
		tags[0], tags[i] = tags[i], tags[0]
		opts.Experimental.ClashAPI.DefaultMode = ManualSelectTag
	}

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
	if err := atomicfile.WriteFile(filepath.Join(path, internal.DebugBoxOptionsFileName), b.Bytes(), fileperm.File); err != nil {
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

// kernelBelow reports whether the kernel version string v is below min.
// Only the first two components (major.minor) are compared, e.g. "5.10" or "4.19.0-android13".
// Returns false if either version string cannot be parsed.
func kernelBelow(v, min string) bool {
	parseKernelMajorMinor := func(s string) (int, int, bool) {
		p := strings.SplitN(s, ".", 3)
		if len(p) < 2 {
			return 0, 0, false
		}
		// Strip non-numeric suffixes (e.g. "19" from "19-android13")
		numericPrefix := func(part string) string {
			for i, r := range part {
				if r < '0' || r > '9' {
					return part[:i]
				}
			}
			return part
		}
		majorStr := numericPrefix(p[0])
		minorStr := numericPrefix(p[1])
		if majorStr == "" || minorStr == "" {
			return 0, 0, false
		}
		major, err := strconv.Atoi(majorStr)
		if err != nil {
			return 0, 0, false
		}
		minor, err := strconv.Atoi(minorStr)
		if err != nil {
			return 0, 0, false
		}
		return major, minor, true
	}
	vMaj, vMin, vok := parseKernelMajorMinor(v)
	mMaj, mMin, mok := parseKernelMajorMinor(min)
	if !vok || !mok {
		return false
	}
	return vMaj < mMaj || (vMaj == mMaj && vMin < mMin)
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
