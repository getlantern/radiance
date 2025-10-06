package vpn

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"

	sbx "github.com/getlantern/sing-box-extensions"
	sbxconstant "github.com/getlantern/sing-box-extensions/constant"
	sbxoption "github.com/getlantern/sing-box-extensions/option"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/internal"
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

	directTag  = "direct-domains"
	directFile = directTag + ".json"
)

// this is the base options that is need for everything to work correctly. this should not be
// changed unless you know what you're doing.
func baseOpts(basePath string) O.Options {
	splitTunnelPath := filepath.Join(basePath, splitTunnelFile)
	directPath := filepath.Join(basePath, directFile)

	// Write the domains to access directly to a file to disk.
	if err := os.WriteFile(directPath, []byte(inlineDirectRuleSet), 0644); err != nil {
		slog.Warn("Failed to write inline direct rule set to file", "path", directPath, "error", err)
	} else {
		slog.Info("Wrote inline direct rule set to file", "path", directPath)
	}

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
						netip.MustParsePrefix("fdf0:f3e1:0fdd::1/126"),
					},
					AutoRoute:   true,
					StrictRoute: true,
					MTU:         1500,
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
				{
					Type: C.RuleSetTypeLocal,
					Tag:  directTag,
					LocalOptions: O.LocalRuleSet{
						Path: directPath,
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

func baseRoutingRules() []O.Rule {
	// routing rules are evaluated in the order they are defined and the first matching rule
	// is applied. So order is important here.
	// DO NOT change the order of the first three rules or things will break. They MUST always
	// be the first three rules.
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
		{ // split tunnel rule
			Type: C.RuleTypeDefault,
			DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{
					RuleSet: []string{splitTunnelTag, directTag},
					//RuleSet: []string{splitTunnelTag},
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
	}
	if common.Platform != "android" {
		rules = append(rules, O.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{
					ProcessPathRegex: lanternRegexForPlatform(),
				},
				RuleAction: O.RuleAction{
					Action: C.RuleActionTypeRoute,
					RouteOptions: O.RouteActionOptions{
						Outbound: "direct",
					},
				},
			},
		})
	}
	rules = append(rules, groupRule(autoAllTag))
	rules = append(rules, groupRule(servers.SGLantern))
	rules = append(rules, groupRule(servers.SGUser))

	// catch-all rule to ensure no fallthrough
	rules = append(rules, O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeReject,
			},
		},
	})

	return rules
}

// buildOptions builds the box options using the config options and user servers.
func buildOptions(group, path string) (O.Options, error) {
	slog.Log(nil, internal.LevelTrace, "Starting buildOptions", "group", group, "path", path)

	opts := baseOpts(path)
	slog.Debug("Base options initialized")

	// update default options and paths
	opts.Experimental.CacheFile.Path = filepath.Join(path, cacheFileName)
	opts.Experimental.ClashAPI.DefaultMode = group

	slog.Log(nil, internal.LevelTrace, "Updated default options and paths",
		"cacheFilePath", opts.Experimental.CacheFile.Path,
		"clashAPIDefaultMode", opts.Experimental.ClashAPI.DefaultMode,
	)

	// platform-specific overrides
	switch common.Platform {
	case "android":
		opts.Route.OverrideAndroidVPN = true
		slog.Debug("Android platform detected, OverrideAndroidVPN set to true")
	case "linux":
		opts.Inbounds[0].Options.(*O.TunInboundOptions).AutoRedirect = true
		slog.Debug("Linux platform detected, AutoRedirect set to true")
	}

	// Load config file
	confPath := filepath.Join(path, common.ConfigFileName)
	slog.Debug("Loading config file", "confPath", confPath)
	configOpts, err := loadConfigOptions(confPath)
	if err != nil {
		slog.Error("Failed to load config options", "error", err)
		return O.Options{}, err
	}

	var lanternTags []string
	switch {
	case len(configOpts.RawMessage) == 0:
		slog.Info("No config found")
	case len(configOpts.Outbounds) == 0 && len(configOpts.Endpoints) == 0:
		slog.Warn("Config loaded but no outbounds or endpoints found")
		fallthrough // Proceed to merge with base options
	default:
		lanternTags = mergeAndCollectTags(&opts, &configOpts)
		slog.Debug("Merged config options", "tags", lanternTags)
	}
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

	// Add auto all outbound
	opts.Outbounds = append(opts.Outbounds, urlTestOutbound(autoAllTag, []string{autoLanternTag, autoUserTag}))

	slog.Debug("Finished building options")
	slog.Log(nil, internal.LevelTrace, "complete options", "options", opts)
	return opts, nil
}

///////////////////////
// Helper functions //
//////////////////////

func loadConfigOptions(confPath string) (O.Options, error) {
	content, err := os.ReadFile(confPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return O.Options{}, fmt.Errorf("read config file: %w", err)
	}
	if len(content) == 0 {
		return O.Options{}, nil
	}
	slog.Log(nil, internal.LevelTrace, "Config file found, unmarshalling", "config", content)
	cfg, err := json.UnmarshalExtendedContext[config.Config](sbx.BoxContext(), content)
	if err != nil {
		return O.Options{}, fmt.Errorf("unmarshal config: %w", err)
	}
	conf := cfg.ConfigResponse

	c := conf
	c.Options.RawMessage = nil
	slog.Log(nil, internal.LevelTrace, "Loaded config", "config", c)

	return conf.Options, nil
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

	// if src.Route != nil {
	// 	dst.Route.Rules = append(dst.Route.Rules, src.Route.Rules...)
	// 	dst.Route.RuleSet = append(dst.Route.RuleSet, src.Route.RuleSet...)
	// }
	// if src.DNS != nil {
	// 	dst.DNS.Servers = append(dst.DNS.Servers, src.DNS.Servers...)
	// }

	var tags []string
	for _, out := range src.Outbounds {
		tags = append(tags, out.Tag)
	}
	for _, ep := range src.Endpoints {
		tags = append(tags, ep.Tag)
	}
	return tags
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
		Type: sbxconstant.TypeMutableURLTest,
		Tag:  tag,
		Options: &sbxoption.MutableURLTestOutboundOptions{
			Outbounds:   outbounds,
			URL:         "http://connectivitycheck.gstatic.com/generate_204",
			Interval:    badoption.Duration(urlTestInterval),
			IdleTimeout: badoption.Duration(urlTestIdleTimeout),
		},
	}
}

func selectorOutbound(group string, outbounds []string) O.Outbound {
	return O.Outbound{
		Type: sbxconstant.TypeMutableSelector,
		Tag:  group,
		Options: &sbxoption.MutableSelectorOutboundOptions{
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

// lanternRegexForPlatform returns the regex patterns to match Lantern process path for the current platform. We want this
// to be as limited as possible to avoid cases where other applications can bypass the VPN.
func lanternRegexForPlatform() []string {
	switch common.Platform {
	case "windows":
		return []string{
			`(?i)^C:\\Program Files( \(x86\))?\\Lantern\\lantern\.exe$`,
			`(?i)^C:\\Users\\[^\\]+\\AppData\\Local\\Programs\\Lantern\\lantern\.exe$`,
			`(?i)^C:\\Users\\[^\\]+\\AppData\\Roaming\\Lantern\\lantern\.exe$`,
		}
	case "darwin":
		return []string{`(?i)^/Lantern.app/Contents/MacOS/lantern$`}
	case "linux":
		return []string{
			`(?i)^/opt/lantern/lantern$`,
			`(?i)^/usr/bin/lantern$`,
			`(?i)^/usr/local/bin/lantern$`,
			`(?i)^/home/.+/(lantern/(bin/)?)?lantern$`,
		}
	default:
		return []string{}
	}
}

// These are embedded domains that should always bypass the VPN.
var inlineDirectRuleSet string = `
{
  "version": 3,
  "rules": [
    {
      "domain_suffix": [
        "iantem.io",
		"a248.e.akamai.net",
		"cloudfront.net",
		"raw.githubusercontent.com"
      ]
    }
  ]
}
`
