package vpn

import (
	"log/slog"
	"net/netip"
	"slices"
	"strings"

	"github.com/miekg/dns"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/getlantern/radiance/common/settings"
)

const (
	fakeIPServerTag = "dns_fakeip"
	fakeIPv4Range   = "198.18.0.0/15"
	fakeIPv6Range   = "fc00::/18"
)

// buildDNSServers returns a list of three DNSServerOptions, a local DNS server
// used for local requests; a remote DNS server (like quad9) for remote websites
// without sharing user private IP; and fake IP dns server, which effectively resolves
// DNS locally while allowing us to route traffic based on domains.
func buildDNSServers() []option.DNSServerOptions {
	local := option.DNSServerOptions{
		Tag:  "dns_local",
		Type: constant.DNSTypeHTTPS,
		Options: &option.RemoteHTTPSDNSServerOptions{
			Path: "/dns-query",
			RemoteTLSDNSServerOptions: option.RemoteTLSDNSServerOptions{
				RemoteDNSServerOptions: option.RemoteDNSServerOptions{
					DNSServerAddressOptions: option.DNSServerAddressOptions{
						Server:     localDNSIP(),
						ServerPort: 443,
					},
				},
			},
		},
	}

	// quad9 doesn't transmit EDNS Client-Subnet data in order to avoid
	// transmitting the user IP  address to the remote site.
	remote := option.DNSServerOptions{
		Type: constant.DNSTypeHTTPS,
		Tag:  "dns_remote",
		Options: &option.RemoteHTTPSDNSServerOptions{
			Path: "/dns-query",
			RemoteTLSDNSServerOptions: option.RemoteTLSDNSServerOptions{
				RemoteDNSServerOptions: option.RemoteDNSServerOptions{
					DNSServerAddressOptions: option.DNSServerAddressOptions{
						Server:     "9.9.9.9",
						ServerPort: 443,
					},
					LocalDNSServerOptions: option.LocalDNSServerOptions{
						DialerOptions: option.DialerOptions{
							Detour: "auto",
						},
					},
				},
			},
		},
	}
	return []option.DNSServerOptions{
		remote,
		local,
		fakeIPServer(),
	}
}

const (
	aliDNS    = "223.5.5.5"
	yandexDNS = "77.88.8.8"
	quad9DNS  = "9.9.9.9"
)

// Locales where AliDNS is used as local DNS server. Note that AliDNS is
// primarily attractive because it is accessible but is understood to return
// results that are DNS poisoned for many sites. This is fine because our
// DNS and routing rules will send that traffic through Lantern proxies,
// and the final DNS resolution will happen on the proxy side.
var aliDNSLocales = map[string]struct{}{
	"FAIR": {},
	"ZHCN": {},
	"CN":   {},
	"IR":   {},
}

func localDNSIP() string {
	locale := settings.GetString(settings.LocaleKey)
	normalizedLocale := normalizeLocale(locale)
	if _, ok := aliDNSLocales[normalizedLocale]; ok {
		slog.Info("Using AliDNS for locale", "locale", locale)
		return aliDNS
	}
	if normalizedLocale == "RU" || normalizedLocale == "RURU" {
		slog.Info("Using Yandex DNS for locale", "locale", locale)
		return yandexDNS
	}
	// default to Quad9
	slog.Info("Using Quad9 for locale", "locale", locale)
	return quad9DNS
}

// normalizeLocale normalizes the locale string by converting it to upper case
// and removing any hyphens or underscores.
func normalizeLocale(locale string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToUpper(locale), "-", ""), "_", "")
}

// buildDNSRules routes address queries to fake-IP so routing keeps domain
// context for both IPv4 and IPv6 destinations.
func buildDNSRules() []option.DNSRule {
	return []option.DNSRule{fakeIPRule(fakeIPServerTag)}
}

// fakeIPServer returns the dual-stack fake-IP DNS server. The IPv6 range keeps
// AAAA answers domain-aware instead of sending raw IPv6 destinations to routing.
func fakeIPServer() option.DNSServerOptions {
	ipv4Prefix := badoption.Prefix(netip.MustParsePrefix(fakeIPv4Range))
	ipv6Prefix := badoption.Prefix(netip.MustParsePrefix(fakeIPv6Range))
	return option.DNSServerOptions{
		Tag:  fakeIPServerTag,
		Type: constant.DNSTypeFakeIP,
		Options: &option.FakeIPDNSServerOptions{
			Inet4Range: &ipv4Prefix,
			Inet6Range: &ipv6Prefix,
		},
	}
}

// fakeIPRule routes address queries to fake-IP so route matching can recover
// the queried domain before evaluating IP fallback rules.
func fakeIPRule(serverTag string) option.DNSRule {
	return option.DNSRule{
		Type: constant.RuleTypeDefault,
		DefaultOptions: option.DefaultDNSRule{
			RawDefaultDNSRule: option.RawDefaultDNSRule{
				QueryType: badoption.Listable[option.DNSQueryType]{
					option.DNSQueryType(dns.TypeA),
					option.DNSQueryType(dns.TypeAAAA),
				},
			},
			DNSRuleAction: option.DNSRuleAction{
				Action: constant.RuleActionTypeRoute,
				RouteOptions: option.DNSRouteActionOptions{
					Server: serverTag,
				},
			},
		},
	}
}

// addFakeIPDNSFallback appends the dual-stack fake-IP path to server DNS
// options while keeping server-supplied rules ahead of the fallback.
func addFakeIPDNSFallback(dnsOptions *option.DNSOptions) {
	serverTag := setFakeIPServer(&dnsOptions.Servers)
	if !hasAddressFakeIPRule(dnsOptions.Rules, serverTag) {
		dnsOptions.Rules = append(dnsOptions.Rules, fakeIPRule(serverTag))
	}
}

// setFakeIPServer returns the fake-IP server tag, replacing any existing
// fake-IP entry with Radiance's dual-stack ranges.
func setFakeIPServer(servers *[]option.DNSServerOptions) string {
	fakeIP := fakeIPServer()
	for i := range *servers {
		if (*servers)[i].Type != constant.DNSTypeFakeIP {
			continue
		}
		if (*servers)[i].Tag == "" {
			(*servers)[i].Tag = fakeIPServerTag
		}
		fakeIP.Tag = (*servers)[i].Tag
		(*servers)[i] = fakeIP
		return fakeIP.Tag
	}
	*servers = append(*servers, fakeIP)
	return fakeIP.Tag
}

// hasAddressFakeIPRule reports whether A and AAAA queries already route to the
// fake-IP server.
func hasAddressFakeIPRule(rules []option.DNSRule, serverTag string) bool {
	for _, rule := range rules {
		opts := rule.DefaultOptions
		if opts.Action == constant.RuleActionTypeRoute &&
			opts.RouteOptions.Server == serverTag &&
			slices.Contains(opts.QueryType, option.DNSQueryType(dns.TypeA)) &&
			slices.Contains(opts.QueryType, option.DNSQueryType(dns.TypeAAAA)) {
			return true
		}
	}
	return false
}
