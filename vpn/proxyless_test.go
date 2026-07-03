package vpn

import (
	"testing"

	O "github.com/sagernet/sing-box/option"
)

func remoteRS(tag, detour string) O.RuleSet {
	return O.RuleSet{
		Type: "remote",
		Tag:  tag,
		RemoteOptions: O.RemoteRuleSet{
			URL:            "https://example.com/" + tag + ".srs",
			DownloadDetour: detour,
		},
	}
}

func TestRepointRuleSetsToProxyless(t *testing.T) {
	opts := &O.Options{
		Route: &O.RouteOptions{
			RuleSet: []O.RuleSet{
				remoteRS("geosite-cn", "direct"),     // direct → repoint
				remoteRS("geosite-ir", ""),           // empty (defaults to direct) → repoint
				remoteRS("geoip-ru", "some-proxy"),   // explicit non-direct detour → leave
				{Type: "local", Tag: "split-tunnel"}, // local, not remote → leave
			},
		},
	}

	if !repointRuleSetsToProxyless(opts) {
		t.Fatal("expected repointed=true")
	}

	rs := opts.Route.RuleSet
	if got := rs[0].RemoteOptions.DownloadDetour; got != proxylessOutboundTag {
		t.Errorf("geosite-cn (direct): detour = %q, want %q", got, proxylessOutboundTag)
	}
	if got := rs[1].RemoteOptions.DownloadDetour; got != proxylessOutboundTag {
		t.Errorf("geosite-ir (empty): detour = %q, want %q", got, proxylessOutboundTag)
	}
	if got := rs[2].RemoteOptions.DownloadDetour; got != "some-proxy" {
		t.Errorf("geoip-ru (proxy): detour = %q, want unchanged %q", got, "some-proxy")
	}
	if got := rs[3].RemoteOptions.DownloadDetour; got != "" {
		t.Errorf("local rule-set should be untouched, detour = %q", got)
	}
}

func TestRepointRuleSetsToProxyless_NoneToRepoint(t *testing.T) {
	opts := &O.Options{
		Route: &O.RouteOptions{
			RuleSet: []O.RuleSet{
				remoteRS("geosite-x", "some-proxy"),
				{Type: "local", Tag: "split-tunnel"},
			},
		},
	}
	if repointRuleSetsToProxyless(opts) {
		t.Error("expected repointed=false when nothing fetches over direct")
	}
}

func TestRepointRuleSetsToProxyless_NilRoute(t *testing.T) {
	if repointRuleSetsToProxyless(&O.Options{}) {
		t.Error("expected repointed=false with nil Route (and no panic)")
	}
}

func TestTagInUse(t *testing.T) {
	opts := &O.Options{
		Outbounds: []O.Outbound{{Tag: "proxy-a"}, {Tag: proxylessOutboundTag}},
		Endpoints: []O.Endpoint{{Tag: "wg0"}},
	}
	if !tagInUse(opts, proxylessOutboundTag) {
		t.Error("expected tagInUse=true for a matching outbound tag")
	}
	if !tagInUse(opts, "wg0") {
		t.Error("expected tagInUse=true for a matching endpoint tag")
	}
	if tagInUse(opts, "absent") {
		t.Error("expected tagInUse=false for an absent tag")
	}
}

// expectProxylessDetour applies the production repoint (repointRuleSetsToProxyless) to
// the given rule-sets in place, so golden assertions in other tests reflect what
// buildOptions produces without duplicating the rewrite predicate here.
func expectProxylessDetour(ruleSets []O.RuleSet) {
	repointRuleSetsToProxyless(&O.Options{Route: &O.RouteOptions{RuleSet: ruleSets}})
}
