package vpn

import (
	"log/slog"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	O "github.com/sagernet/sing-box/option"
)

// proxylessOutboundTag is the tag of the proxyless outbound used as the
// download_detour for remote rule-sets fetched at cold start.
const proxylessOutboundTag = "proxyless"

// proxylessProbeHosts are the hosts the smart dialer probes to find a working
// (resolver + TLS-fragmentation) strategy. The winning strategy is then applied
// to every dial through the outbound, whatever the actual rule-set host — so
// these need only be representative of the CDNs the rule-sets are served from
// (jsDelivr's multi-CDN front and GitHub raw). Validated 2026-06-30 from CN
// residential: this Fastly/jsDelivr/githubusercontent family is throttled at the
// SNI layer, which TLS-record fragmentation reliably defeats (full .srs in
// 3-5s). s3.amazonaws.com is deliberately excluded — its throttle is rate/flow-
// based, not SNI, so fragmentation doesn't help it (0/6 completed in testing).
var proxylessProbeHosts = []string{
	"fastly.jsdelivr.net",
	"cdn.jsdelivr.net",
	"raw.githubusercontent.com",
}

// proxylessOutbound builds the proxyless outbound: the outline-sdk smart
// dialer reaching rule-set hosts directly with DoH resolution + TLS-record
// fragmentation, defeating SNI-based DPI without a proxy server. Used only as a
// download_detour (not a user-selectable proxy), so a cold-start rule-set fetch
// survives DPI interference where a plain `direct` fetch is throttled to death.
func proxylessOutbound() O.Outbound {
	return O.Outbound{
		Type: lbC.TypeOutline,
		Tag:  proxylessOutboundTag,
		Options: &lbO.OutboundOutlineOptions{
			Domains:     proxylessProbeHosts,
			TestTimeout: "5s",
			// DoH resolvers (un-poisonable), incl. an in-China one (AliDNS) so
			// the strategy search can resolve from behind the GFW.
			DNSResolvers: []lbO.DNSEntryConfig{
				{HTTPS: &lbO.HTTPSEntryConfig{Name: "cloudflare-dns.com"}},
				{HTTPS: &lbO.HTTPSEntryConfig{Name: "8.8.8.8"}},
				{HTTPS: &lbO.HTTPSEntryConfig{Name: "223.5.5.5"}},
			},
			// "" = plain direct first; the split/frag strategies break up the
			// TLS ClientHello so the GFW can't SNI-match (and so can't throttle).
			TLS: []string{"", "split:1", "split:2,20*5", "tlsfrag:1"},
		},
	}
}

// repointRuleSetsToProxyless rewrites every remote rule-set fetching over `direct`
// (the DPI-exposed path) to fetch through the proxyless outbound instead. Applies
// to all remote rule-sets regardless of region — any of them can be throttled at
// cold start behind a censor, and the proxyless tries plain direct first, so
// uncensored fetches are unaffected. Returns true if any rule-set was repointed.
func repointRuleSetsToProxyless(opts *O.Options) bool {
	if opts.Route == nil {
		return false
	}
	var repointed bool
	for i := range opts.Route.RuleSet {
		rs := &opts.Route.RuleSet[i]
		if rs.Type != "remote" {
			continue
		}
		if d := rs.RemoteOptions.DownloadDetour; d == "" || d == "direct" {
			rs.RemoteOptions.DownloadDetour = proxylessOutboundTag
			repointed = true
			slog.Info("Repointing rule-set fetch to proxyless outbound",
				slog.String("tag", rs.Tag), slog.String("url", rs.RemoteOptions.URL))
		}
	}
	return repointed
}

// tagInUse reports whether any outbound or endpoint already uses tag. sing-box
// requires tags unique across both, so the proxyless injection checks both before
// adding its outbound.
func tagInUse(opts *O.Options, tag string) bool {
	for _, o := range opts.Outbounds {
		if o.Tag == tag {
			return true
		}
	}
	for _, e := range opts.Endpoints {
		if e.Tag == tag {
			return true
		}
	}
	return false
}
