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

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/common/settings"
)

// buildDNSServers returns dns_remote, dns_local with the servers its pool
// tries, and dns_fakeip. dns_local and its servers have no detour: they dial
// directly, with no dependency on any outbound.
func buildDNSServers() []option.DNSServerOptions {
	ipv4Prefix := badoption.Prefix(netip.MustParsePrefix("198.18.0.0/15"))
	fakeIP := option.DNSServerOptions{
		Tag:  "dns_fakeip",
		Type: constant.DNSTypeFakeIP,
		Options: &option.FakeIPDNSServerOptions{
			Inet4Range: &ipv4Prefix,
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
					RawLocalDNSServerOptions: option.RawLocalDNSServerOptions{
						DialerOptions: option.DialerOptions{
							Detour: "auto",
						},
					},
				},
			},
		},
	}
	servers := []option.DNSServerOptions{remote}
	servers = append(servers, localDNSServers(localResolvers())...)
	return append(servers, fakeIP)
}

// dohResolver is a DNS-over-HTTPS resolver dialed at ip. serverName, when
// set, is the TLS SNI and Host sent to it; otherwise the certificate must
// cover the IP.
type dohResolver struct {
	name       string
	ip         string
	serverName string
}

var (
	cloudflareDoH = dohResolver{name: "cloudflare", ip: "1.1.1.1"}
	googleDoH     = dohResolver{name: "google", ip: "8.8.8.8"}
	aliDoH        = dohResolver{name: "alidns", ip: "223.5.5.5"}
	yandexDoH     = dohResolver{name: "yandex", ip: "77.88.8.8"}
	// Cloudflare anycast edges answer cloudflare-dns.com DoH on the same IPs
	// that serve millions of sites, so blocking them costs a censor that
	// collateral. One from each of three of the ranges in lantern-cloud's
	// cloudflareDoHEdges, which mirrors flint's pool.
	cloudflareEdgeDoH = []dohResolver{
		{name: "cf_edge_104_16", ip: "104.16.249.249", serverName: "cloudflare-dns.com"},
		{name: "cf_edge_172_64", ip: "172.67.68.111", serverName: "cloudflare-dns.com"},
		{name: "cf_edge_162_158", ip: "162.159.136.232", serverName: "cloudflare-dns.com"},
	}
)

// Locales where AliDNS is tried first. AliDNS is attractive because it is
// reachable there, though it returns poisoned answers for many sites. That is
// acceptable: dns_local resolves only what goes direct, and routing sends the
// rest through Lantern proxies, which resolve it on the proxy side.
var aliDNSLocales = map[string]struct{}{
	"FAIR": {},
	"ZHCN": {},
	"CN":   {},
	"IR":   {},
}

// localResolvers returns the resolvers dns_local tries, in order, for the
// configured locale. The pool moves on from one that is blocked within a
// stagger, so each list leads with what usually works there. Censored
// locales fall back to the Cloudflare edges, not to 1.1.1.1 or 8.8.8.8,
// which censors block by address. Google forwards EDNS Client-Subnet, unlike
// Quad9, which this replaced because some networks block its DoH; dns_local
// resolves only what goes direct, so that is the trade made.
func localResolvers() []dohResolver {
	locale := settings.GetString(settings.LocaleKey)
	normalizedLocale := normalizeLocale(locale)
	var resolvers []dohResolver
	if _, ok := aliDNSLocales[normalizedLocale]; ok {
		resolvers = slices.Concat([]dohResolver{aliDoH}, cloudflareEdgeDoH)
	} else if normalizedLocale == "RU" || normalizedLocale == "RURU" {
		// Russian ISPs filter AliDNS; Yandex is reachable from all of them.
		resolvers = slices.Concat([]dohResolver{yandexDoH}, cloudflareEdgeDoH)
	} else {
		resolvers = slices.Concat([]dohResolver{cloudflareDoH, googleDoH}, cloudflareEdgeDoH)
	}
	names := make([]string, len(resolvers))
	for i, r := range resolvers {
		names[i] = r.name
	}
	slog.Info("Local DNS resolvers for locale", "locale", locale, "resolvers", names)
	return resolvers
}

// localDNSServers returns dns_local, a pool over resolvers, followed by a
// DoH server for each resolver. None has a detour: each dials directly
// through the system network, with no dependency on any outbound.
func localDNSServers(resolvers []dohResolver) []option.DNSServerOptions {
	pool := &lbO.DNSPoolServerOptions{}
	servers := []option.DNSServerOptions{{Tag: "dns_local", Type: lbC.TypeDNSPool, Options: pool}}
	for _, r := range resolvers {
		tag := "dns_local_" + r.name
		pool.Servers = append(pool.Servers, tag)
		options := &option.RemoteHTTPSDNSServerOptions{
			Path: "/dns-query",
			RemoteTLSDNSServerOptions: option.RemoteTLSDNSServerOptions{
				RemoteDNSServerOptions: option.RemoteDNSServerOptions{
					DNSServerAddressOptions: option.DNSServerAddressOptions{
						Server:     r.ip,
						ServerPort: 443,
					},
				},
			},
		}
		if r.serverName != "" {
			options.TLS = &option.OutboundTLSOptions{Enabled: true, ServerName: r.serverName}
		}
		servers = append(servers, option.DNSServerOptions{Tag: tag, Type: constant.DNSTypeHTTPS, Options: options})
	}
	return servers
}

// normalizeLocale normalizes the locale string by converting it to upper case
// and removing any hyphens or underscores.
func normalizeLocale(locale string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToUpper(locale), "-", ""), "_", "")
}

// buildDNSRules routes A queries to the fake-IP server and suppresses AAAA so
// applications fall back to IPv4.
func buildDNSRules() []option.DNSRule {
	return []option.DNSRule{suppressAAAARule(), fakeipRule()}

}

func fakeipServer() option.DNSServerOptions {
	ipv4Prefix := badoption.Prefix(netip.MustParsePrefix("198.18.0.0/15"))
	return option.DNSServerOptions{
		Tag:  "dns_fakeip",
		Type: constant.DNSTypeFakeIP,
		Options: &option.FakeIPDNSServerOptions{
			Inet4Range: &ipv4Prefix,
		},
	}

}

func fakeipRule() option.DNSRule {
	return option.DNSRule{
		Type: constant.RuleTypeDefault,
		DefaultOptions: option.DefaultDNSRule{
			RawDefaultDNSRule: option.RawDefaultDNSRule{
				QueryType: badoption.Listable[option.DNSQueryType]{option.DNSQueryType(dns.TypeA)},
			},
			DNSRuleAction: option.DNSRuleAction{
				Action: constant.RuleActionTypeRoute,
				RouteOptions: option.DNSRouteActionOptions{
					Server: "dns_fakeip",
				},
			},
		},
	}
}

// suppressAAAARule answers AAAA queries with NODATA (NOERROR and no records) so
// applications see no IPv6 address and connect over IPv4.
func suppressAAAARule() option.DNSRule {
	noError := option.DNSRCode(dns.RcodeSuccess)
	return option.DNSRule{
		Type: constant.RuleTypeDefault,
		DefaultOptions: option.DefaultDNSRule{
			RawDefaultDNSRule: option.RawDefaultDNSRule{
				QueryType: badoption.Listable[option.DNSQueryType]{option.DNSQueryType(dns.TypeAAAA)},
			},
			DNSRuleAction: option.DNSRuleAction{
				Action:            constant.RuleActionTypePredefined,
				PredefinedOptions: option.DNSRouteActionPredefined{Rcode: &noError},
			},
		},
	}
}
