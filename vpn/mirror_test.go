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

func TestRepointRuleSetsToMirror(t *testing.T) {
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

	if !repointRuleSetsToMirror(opts) {
		t.Fatal("expected repointed=true")
	}

	rs := opts.Route.RuleSet
	if got := rs[0].RemoteOptions.DownloadDetour; got != mirrorOutboundTag {
		t.Errorf("geosite-cn (direct): detour = %q, want %q", got, mirrorOutboundTag)
	}
	if got := rs[1].RemoteOptions.DownloadDetour; got != mirrorOutboundTag {
		t.Errorf("geosite-ir (empty): detour = %q, want %q", got, mirrorOutboundTag)
	}
	if got := rs[2].RemoteOptions.DownloadDetour; got != "some-proxy" {
		t.Errorf("geoip-ru (proxy): detour = %q, want unchanged %q", got, "some-proxy")
	}
	if got := rs[3].RemoteOptions.DownloadDetour; got != "" {
		t.Errorf("local rule-set should be untouched, detour = %q", got)
	}
}

func TestRepointRuleSetsToMirror_NoneToRepoint(t *testing.T) {
	opts := &O.Options{
		Route: &O.RouteOptions{
			RuleSet: []O.RuleSet{
				remoteRS("geosite-x", "some-proxy"),
				{Type: "local", Tag: "split-tunnel"},
			},
		},
	}
	if repointRuleSetsToMirror(opts) {
		t.Error("expected repointed=false when nothing fetches over direct")
	}
}

func TestRepointRuleSetsToMirror_NilRoute(t *testing.T) {
	if repointRuleSetsToMirror(&O.Options{}) {
		t.Error("expected repointed=false with nil Route (and no panic)")
	}
}

func TestTagInUse(t *testing.T) {
	opts := &O.Options{
		Outbounds: []O.Outbound{{Tag: "proxy-a"}, {Tag: mirrorOutboundTag}},
		Endpoints: []O.Endpoint{{Tag: "wg0"}},
	}
	if !tagInUse(opts, mirrorOutboundTag) {
		t.Error("expected tagInUse=true for a matching outbound tag")
	}
	if !tagInUse(opts, "wg0") {
		t.Error("expected tagInUse=true for a matching endpoint tag")
	}
	if tagInUse(opts, "absent") {
		t.Error("expected tagInUse=false for an absent tag")
	}
}

// expectMirrorDetour applies the production repoint (repointRuleSetsToMirror) to
// the given rule-sets in place, so golden assertions in other tests reflect what
// buildOptions produces without duplicating the rewrite predicate here.
func expectMirrorDetour(ruleSets []O.RuleSet) {
	repointRuleSetsToMirror(&O.Options{Route: &O.RouteOptions{RuleSet: ruleSets}})
}
