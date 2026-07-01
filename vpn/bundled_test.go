package vpn

import (
	"bytes"
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
		// A malicious server-provided tag with path separators must be rejected,
		// not written outside the rulesets dir.
		{Type: C.RuleSetTypeRemote, Tag: "geosite-cn/../../evil", Format: C.RuleSetFormatBinary,
			RemoteOptions: O.RemoteRuleSet{URL: "https://x/evil.srs", DownloadDetour: "direct"}},
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
	if !bytes.Equal(data, bundledGeositeCN) {
		t.Errorf("written file (%d bytes) != embedded rule-set (%d bytes)", len(data), len(bundledGeositeCN))
	}

	// Non-geosite remote, existing local, and the unsafe-tag rule-sets are untouched.
	if opts.Route.RuleSet[1].Type != C.RuleSetTypeRemote {
		t.Errorf("ai-domains should stay remote, got %q", opts.Route.RuleSet[1].Type)
	}
	if opts.Route.RuleSet[2].Tag != "split-tunnel" || opts.Route.RuleSet[2].Type != C.RuleSetTypeLocal {
		t.Errorf("split-tunnel should be untouched")
	}
	if opts.Route.RuleSet[3].Type != C.RuleSetTypeRemote {
		t.Errorf("unsafe tag should be left remote (not bundled), got %q", opts.Route.RuleSet[3].Type)
	}
	// Nothing escaped the rulesets dir.
	if escaped, _ := filepath.Glob(filepath.Join(base, "*.srs")); len(escaped) != 0 {
		t.Errorf("files written outside rulesets/: %v", escaped)
	}
}
