package vpn

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	lcommon "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"
	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
)

const (
	AutoSelectTag   = "auto"
	ManualSelectTag = "manual"

	urlTestInterval    = 3 * time.Minute
	urlTestIdleTimeout = 15 * time.Minute

	cacheID              = "lantern"
	cacheFileName        = "lantern.cache"
	cacheClearMarkerName = "lantern.cache.clear"
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
	// default probe URL for each specific outbound, allowing the server to
	// detect which proxies successfully connected.
	BanditURLOverrides map[string]string `json:"bandit_url_overrides,omitempty"`
	// SelectionHistorySeed seeds the tunnel's AutoSelectHistoryStorage
	// at startup with the latest persisted snapshot per tag.
	SelectionHistorySeed map[string]lbA.TagHistory `json:"tag_history"`
}

// isGlobalIPv6 reports whether ip is in 2000::/3. Not net.IP.IsGlobalUnicast,
// which also accepts ULA — ULA-only interfaces (Tailscale, corp VPNs) don't
// indicate real public-v6 connectivity. Reserved-but-in-range prefixes like
// 2001:db8::/32 (documentation) and 2002::/16 (6to4) still return true; their
// presence on a real interface still signals "system is configured for v6".
func isGlobalIPv6(ip net.IP) bool {
	if ip.To4() != nil {
		return false
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return false
	}
	// 2000::/3: first three bits are 001, first-byte mask 0xe0 == 0x20.
	return ip16[0]&0xe0 == 0x20
}

// ifaceSnapshot is the test seam: the data hasGlobalIPv6 reads per interface,
// decoupled from net.Interface so tests can simulate any network config.
type ifaceSnapshot struct {
	name  string // logging only
	flags net.Flags
	addrs []net.Addr
}

// snapshotInterfaces is the production snapshot provider. Tests inject their own.
func snapshotInterfaces() ([]ifaceSnapshot, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("net.Interfaces: %w", err)
	}
	out := make([]ifaceSnapshot, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			// Per-interface failure → empty addrs, keep going.
			addrs = nil
		}
		out = append(out, ifaceSnapshot{name: iface.Name, flags: iface.Flags, addrs: addrs})
	}
	return out, nil
}

// hasGlobalIPv6 gates the TUN v6 ULA: enable when the system has real v6,
// disable on v4-only networks where it's been observed to break things we
// haven't narrowed down. Called per tunnel start; not cached.
func hasGlobalIPv6() bool {
	return hasGlobalIPv6Using(snapshotInterfaces)
}

// hasGlobalIPv6Using is the testable core; production wraps via snapshotInterfaces.
func hasGlobalIPv6Using(getSnapshots func() ([]ifaceSnapshot, error)) bool {
	snaps, err := getSnapshots()
	if err != nil {
		return false
	}
	for _, s := range snaps {
		if s.flags&net.FlagUp == 0 || s.flags&net.FlagLoopback != 0 {
			continue
		}
		for _, a := range s.addrs {
			// Addrs() may return *net.IPNet or *net.IPAddr depending on platform.
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			if isGlobalIPv6(ip) {
				return true
			}
		}
	}
	return false
}

// baseOpts returns the minimum sing-box options required for the tunnel to
// function. Do not modify without understanding the downstream effects.
func baseOpts(basePath string) O.Options {
	cacheFile := cacheFilePath(basePath)
	inbounds := baseInbounds()

	opts := O.Options{
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
		Inbounds: inbounds,
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
			RuleSet:             splitTunnelRuleSet(basePath),
			DefaultDomainResolver: &O.DomainResolveOptions{
				Server: "dns_local",
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

	// OverrideAndroidVPN is meaningless without a TUN inbound, so gate on its
	// presence — the novpn build has none.
	if common.Platform == "android" && hasTunInbound(inbounds) {
		opts.Route.OverrideAndroidVPN = true
	}
	return opts
}

func hasTunInbound(inbounds []O.Inbound) bool {
	for _, in := range inbounds {
		if in.Type == "tun" {
			return true
		}
	}
	return false
}

func baseRoutingRules() []O.Rule {
	// routing rules are evaluated in the order they are defined and the first matching rule
	// is applied. So order is important here.
	// The rules MUST be in this order for proper behavior:
	// 1.    Enable traffic sniffing
	// 2.    Hijack DNS to allow sing-box to handle DNS requests
	// 3.    Route bypass proxy traffic directly (for kindling connections)
	// 4.    Route private IPs to direct outbound
	// 5.    Split tunnel rule (user-configurable)
	// 6.    Smart-routing, ad-block, and config-file rules (added in buildOptions).
	// 7.    Reject QUIC (UDP/443) for any UDP/443 not already matched above; placed
	//       here so split-tunnel, smart-routed, and config-routed direct paths keep
	//       their QUIC. Added in buildOptions.
	// 8.    Reject IPv6 (::/0), only when the TUN captured a v6 address; placed here so
	//       direct-routed v6 is preserved while remaining IPv6 fails fast. Added in buildOptions.
	// 9,10. Group rules for auto and manual selector modes (added in buildOptions).
	// 11.   Catch-all blocking rule (added in buildOptions). Traffic not covered
	//       by previous rules must not automatically bypass the VPN.
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
	}
	rules = append(rules, bypassRoutingRules()...)
	rules = append(rules, O.Rule{
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
	})
	rules = append(rules, splitTunnelRoutingRules()...)
	return rules
}

func cacheFilePath(basePath string) string {
	return filepath.Join(basePath, cacheFileName)
}

func cacheClearMarkerPath(basePath string) string {
	return filepath.Join(basePath, cacheClearMarkerName)
}

// removeCacheFile deletes the tunnel cache if it exists.
func removeCacheFile(basePath string) error {
	err := os.Remove(cacheFilePath(basePath))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing cache file: %w", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		slog.Info("removed cache file", "path", cacheFilePath(basePath))
	}
	return nil
}

// writeCacheClearMarker records that cache removal must be retried on the next
// tunnel start.
func writeCacheClearMarker(basePath string) error {
	slog.Debug("writing cache clear marker", "path", cacheClearMarkerPath(basePath))
	if err := atomicfile.WriteFile(cacheClearMarkerPath(basePath), nil, fileperm.File); err != nil {
		return fmt.Errorf("writing cache clear marker: %w", err)
	}
	return nil
}

// consumeCacheClearMarker applies a deferred cache clear during tunnel start
// and removes the marker after a successful cache deletion.
func consumeCacheClearMarker(basePath string) error {
	marker := cacheClearMarkerPath(basePath)
	if _, err := os.Stat(marker); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	if err := removeCacheFile(basePath); err != nil {
		return err
	}
	os.Remove(marker) // we can safely ignore errors here
	return nil
}

// rejectQUICRule rejects UDP/443 to force HTTP/2-over-TCP fallback. Standard
// pattern in TUN-mode sing-box clients (Hiddify, NekoBox, Clash Meta) because
// QUIC-over-TCP-outbound stacks two loss-recovery/congestion regimes — strictly
// worse than letting Chrome drop to HTTP/2. Caller is responsible for placing
// this AFTER all rules that may route to direct (split tunnel, smart routing,
// config file) so direct-routed domains keep their QUIC, and BEFORE the
// proxy selectors so QUIC bound for a proxy is rejected.
func rejectQUICRule() O.Rule {
	return O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{
				Network: []string{"udp"},
				Port:    []uint16{443},
			},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeReject,
			},
		},
	}
}

// rejectIPv6Rule rejects IPv6 destinations that reach this late in route order.
// The default reject method fails fast; "drop" would blackhole and stall.
func rejectIPv6Rule() O.Rule {
	return O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{
				IPCIDR: []string{"::/0"},
			},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeReject,
			},
		},
	}
}

// tunHasIPv6 reports whether a TUN inbound was given an IPv6 address, meaning v6
// is captured into the tunnel and must be rejected rather than left to bypass.
func tunHasIPv6(opts O.Options) bool {
	for _, in := range opts.Inbounds {
		t, ok := in.Options.(*O.TunInboundOptions)
		if !ok {
			continue
		}
		for _, addr := range t.Address {
			if addr.Addr().Is6() {
				return true
			}
		}
	}
	return false
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

	// A caller-supplied Dir (e.g. /tmp from a Linux-targeting config) may not
	// be writable on the device; always point WATER outbounds at the app's
	// managed data directory instead.
	waterDir := filepath.Join(bOptions.BasePath, "water")
	for i := range opts.Outbounds {
		if opts.Outbounds[i].Type == lbC.TypeWATER {
			if waterOpts, ok := opts.Outbounds[i].Options.(*lbO.WATEROutboundOptions); ok {
				cp := *waterOpts
				cp.Dir = waterDir
				opts.Outbounds[i].Options = &cp
			}
		}
	}

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

	opts.Route.Rules = append(opts.Route.Rules, rejectQUICRule())
	if tunHasIPv6(opts) {
		opts.Route.Rules = append(opts.Route.Rules, rejectIPv6Rule())
	}

	// add mode selector outbounds and rules
	opts.Outbounds = append(opts.Outbounds, urlTestOutbound(AutoSelectTag, tags, bOptions.BanditURLOverrides))
	opts.Outbounds = append(opts.Outbounds, selectorOutbound(ManualSelectTag, tags))
	opts.Route.Rules = append(opts.Route.Rules, selectModeRule(AutoSelectTag))
	opts.Route.Rules = append(opts.Route.Rules, selectModeRule(ManualSelectTag))

	// catch-all rule so traffic cannot fall through
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
		dst.Route.DefaultDomainResolver = src.Route.DefaultDomainResolver
	}
	// overwrite base DNS options with config from src (server)
	if src.DNS != nil {
		dns := *src.DNS
		dns.Servers = slices.Clone(src.DNS.Servers)
		dns.Rules = slices.Clone(src.DNS.Rules)
		addFakeIPDNSFallback(&dns)
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
		Type: lbC.TypeMutableAutoSelect,
		Tag:  tag,
		Options: &lbO.MutableAutoSelectOutboundOptions{
			Outbounds:                 outbounds,
			URL:                       "https://google.com/generate_204",
			URLOverrides:              urlOverrides,
			BackgroundIntervalSeconds: uint32(urlTestInterval / time.Second),
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
