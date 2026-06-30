package vpn

import (
	"os"
	"path/filepath"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
)

func TestBundleGeositeRuleSets(t *testing.T) {
	base := t.TempDir()
	opts := &O.Options{Route: &O.RouteOptions{}}
	opts.Route.RuleSet = []O.RuleSet{
		{Type: C.RuleSetTypeRemote, Tag: "geosite-cn2", Format: C.RuleSetFormatBinary,
			RemoteOptions: O.RemoteRuleSet{URL: "https://s3.amazonaws.com/lantern/geosite-cn.srs", DownloadDetour: "direct"}},
		{Type: C.RuleSetTypeRemote, Tag: "ai-domains", Format: C.RuleSetFormatBinary,
			RemoteOptions: O.RemoteRuleSet{URL: "https://example.com/ai.srs", DownloadDetour: "direct"}},
		{Type: C.RuleSetTypeLocal, Tag: "split-tunnel", Format: C.RuleSetFormatSource,
			LocalOptions: O.LocalRuleSet{Path: "/x/split.json"}},
	}

	bundled := bundleGeositeRuleSets(opts, base)
	if len(bundled) != 1 || bundled[0] != "geosite-cn2" {
		t.Fatalf("bundled = %v, want [geosite-cn2]", bundled)
	}

	// geosite-cn2 → local/binary, pointing at a written copy of the embedded .srs.
	cn := opts.Route.RuleSet[0]
	if cn.Type != C.RuleSetTypeLocal || cn.Format != C.RuleSetFormatBinary {
		t.Errorf("geosite-cn2 not converted: type=%q format=%q", cn.Type, cn.Format)
	}
	want := filepath.Join(base, "rulesets", "geosite-cn2.srs")
	if cn.LocalOptions.Path != want {
		t.Errorf("path = %q, want %q", cn.LocalOptions.Path, want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("bundled file not written: %v", err)
	}
	if len(data) == 0 || len(data) != len(bundledGeositeCN) {
		t.Errorf("written %d bytes, embedded %d", len(data), len(bundledGeositeCN))
	}

	// A non-geosite remote rule-set and an existing local rule-set are untouched.
	if opts.Route.RuleSet[1].Type != C.RuleSetTypeRemote {
		t.Errorf("ai-domains should stay remote, got %q", opts.Route.RuleSet[1].Type)
	}
	if opts.Route.RuleSet[2].Tag != "split-tunnel" || opts.Route.RuleSet[2].Type != C.RuleSetTypeLocal {
		t.Errorf("split-tunnel should be untouched")
	}
}
