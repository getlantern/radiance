package vpn

import (
	"log/slog"
	"net/netip"

	"github.com/getlantern/radiance/common"
	"github.com/miekg/dns"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/spf13/viper"
)

// buildDNSServers returns a list of of three DNSServerOptions, a local DNS server
// used for local requests (e.g. government domains); a remote DNS server (like quad9)
// for remote websites without sharing user private IP; and fake IP dns server, which
// generates a random IP and uses it when sending the DNS requests.
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
	ipv4Prefix := badoption.Prefix(netip.MustParsePrefix("198.18.0.0/15"))
	ipv6Prefix := badoption.Prefix(netip.MustParsePrefix("fc00::/18"))
	fakeIP := option.DNSServerOptions{
		Tag:  "dns_fakeip",
		Type: constant.DNSTypeFakeIP,
		Options: &option.FakeIPDNSServerOptions{
			Inet4Range: &ipv4Prefix,
			Inet6Range: &ipv6Prefix,
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
		fakeIP,
	}
}

// Locales where AliDNS is used as local DNS server. Note that AliDNS is
// primarily attractive because it is accessible but is understood to return
// results that are DNS poisoned for many sites. This is fine because our
// DNS and routing rules will send that traffic through Lantern proxies,
// and the final DNS resolution will happen on the proxy side.
var aliDNSLocales = map[string]struct{}{
	"FAIR": {},
	"ZHCN": {},
	"RURU": {},
}

func localDNSIP() string {
	// First, normalize the locale to upper case and remove any hyphens or underscores.
	locale := viper.GetString(common.LocaleKey)
	normalizedLocale := normalizeLocale(locale)
	if _, ok := aliDNSLocales[normalizedLocale]; ok {
		slog.Info("Using AliDNS for locale", "locale", locale)
		// AliDNS
		return "223.5.5.5"
	}
	// Quad9, which is more privacy preserving by doing things such as
	// not sending EDNS Client-Subnet data
	slog.Info("Using Quad9 for locale", "locale", locale)
	return "9.9.9.9"
}

func normalizeLocale(locale string) string {
	normalizedLocale := ""
	for _, r := range locale {
		if r != '-' && r != '_' {
			if r >= 'a' && r <= 'z' {
				normalizedLocale += string(r - ('a' - 'A'))
			} else {
				normalizedLocale += string(r)
			}
		}
	}
	return normalizedLocale
}

// buildDNSRules look for rule sets that have a DNS Server value registered,
// map them and generate a list of DNS rules. It also adds an additional DNS rule
// for fake ip.
func buildDNSRules() []option.DNSRule {
	dnsRules := make([]option.DNSRule, 0)
	dnsRules = append(dnsRules, option.DNSRule{
		Type: constant.RuleTypeDefault,
		DefaultOptions: option.DefaultDNSRule{
			RawDefaultDNSRule: option.RawDefaultDNSRule{
				QueryType: badoption.Listable[option.DNSQueryType]{option.DNSQueryType(dns.TypeA), option.DNSQueryType(dns.TypeAAAA)},
			},
			DNSRuleAction: option.DNSRuleAction{
				Action: constant.RuleActionTypeRoute,
				RouteOptions: option.DNSRouteActionOptions{
					Server: "dns_fakeip",
				},
			},
		},
	})

	return dnsRules
}
