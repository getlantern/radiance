package vpn

import (
	"log/slog"
	"strings"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	O "github.com/sagernet/sing-box/option"
)

// mirrorOutboundTag is the tag of the proxyless "mirror" outbound used as the
// download_detour for routing rule-sets fetched at cold start (see engineering#3657).
const mirrorOutboundTag = "mirror"

// geositeMirrorHosts are the hosts the smart dialer probes to find a working
// (resolver + TLS-fragmentation) strategy. Validated 2026-06-30 from CN
// residential: the jsdelivr/Fastly + githubusercontent family is throttled at
// the SNI layer, which TLS-record fragmentation reliably defeats (full .srs in
// 3-5s). s3.amazonaws.com is deliberately EXCLUDED — its throttle is rate/flow-
// based, NOT SNI, so fragmentation does not help it (0/6 completed in testing).
// The rule-set URL must therefore point at one of these hosts, not s3.
var geositeMirrorHosts = []string{
	"fastly.jsdelivr.net",
	"cdn.jsdelivr.net",
	"raw.githubusercontent.com",
}

// mirrorOutbound builds the proxyless "mirror" outbound: the outline-sdk smart
// dialer reaching the rule-set mirrors directly with DoH resolution + TLS-record
// fragmentation, defeating SNI-based DPI without a proxy server. Used only as a
// download_detour (not a user-selectable proxy), so a cold-start rule-set fetch
// survives GFW interference where a plain `direct` fetch is throttled to death.
func mirrorOutbound() O.Outbound {
	return O.Outbound{
		Type: lbC.TypeOutline,
		Tag:  mirrorOutboundTag,
		Options: &lbO.OutboundOutlineOptions{
			Domains:     geositeMirrorHosts,
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

// repointRuleSetsToMirror rewrites remote rule-sets that fetch over `direct`
// (the GFW-exposed path) to use the mirror outbound instead. Scoped to the
// geosite-cn* tags — the ones fetched at cold start from foreign CDNs the GFW
// throttles. Returns true if any rule-set was repointed.
func repointRuleSetsToMirror(opts *O.Options) bool {
	var repointed bool
	for i := range opts.Route.RuleSet {
		rs := &opts.Route.RuleSet[i]
		if rs.Type != "remote" || !strings.HasPrefix(rs.Tag, "geosite-cn") {
			continue
		}
		if d := rs.RemoteOptions.DownloadDetour; d == "" || d == "direct" {
			rs.RemoteOptions.DownloadDetour = mirrorOutboundTag
			repointed = true
			slog.Info("Repointing rule-set fetch to mirror outbound",
				slog.String("tag", rs.Tag), slog.String("url", rs.RemoteOptions.URL))
		}
	}
	return repointed
}
