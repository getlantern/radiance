package vpn

import (
	_ "embed"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
)

// bundledGeositeCN is Lantern's custom compiled geosite-cn rule-set, embedded in
// the client so cold start needs no network fetch. Refresh by replacing the
// embedded file with a newer upstream build and rebuilding.
//
//go:embed rulesets/geosite-cn.srs
var bundledGeositeCN []byte

// bundleGeositeRuleSets converts server-pushed geosite-cn* REMOTE rule-sets into
// LOCAL rule-sets backed by the .srs bundled above, removing the tunnel-start
// network fetch: no CDN reliably serves it from behind the GFW at our scale, so
// the bundled copy is the floor — the rule-set always loads and a blocked fetch
// can't stall startup. A later refresh layer may overwrite the on-disk copy to
// keep it current between releases. Returns the tags that were bundled.
func bundleGeositeRuleSets(opts *O.Options, basePath string) []string {
	dir := filepath.Join(basePath, "rulesets")
	var bundled []string
	for i := range opts.Route.RuleSet {
		rs := &opts.Route.RuleSet[i]
		if rs.Type != C.RuleSetTypeRemote || !strings.HasPrefix(rs.Tag, "geosite-cn") {
			continue
		}
		// The tag is server-provided and becomes a filename; require a bare
		// filename (reject path separators / "..") so it can neither escape nor
		// nest under the rulesets dir.
		if filepath.Base(rs.Tag) != rs.Tag {
			slog.Warn("bundle geosite: unsafe rule-set tag; leaving remote rule-set",
				slog.String("tag", rs.Tag))
			continue
		}
		path := filepath.Join(dir, rs.Tag+".srs")
		if !strings.HasPrefix(path, dir+string(os.PathSeparator)) { // defense in depth
			slog.Warn("bundle geosite: unsafe rule-set tag; leaving remote rule-set",
				slog.String("tag", rs.Tag))
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("bundle geosite: mkdir failed; leaving remote rule-set",
				slog.String("tag", rs.Tag), slog.Any("error", err))
			continue
		}
		// Write only when missing or a different size: buildOptions runs on every
		// connect, and re-fsyncing the rule-set each time is wasteful. The embedded
		// rule-set's length changes on app upgrade, so a size mismatch reliably
		// signals "needs rewrite".
		if fi, err := os.Stat(path); err != nil || fi.Size() != int64(len(bundledGeositeCN)) {
			if err := atomicfile.WriteFile(path, bundledGeositeCN, fileperm.File); err != nil {
				slog.Warn("bundle geosite: write failed; leaving remote rule-set",
					slog.String("tag", rs.Tag), slog.Any("error", err))
				continue
			}
		}
		*rs = O.RuleSet{
			Type:         C.RuleSetTypeLocal,
			Tag:          rs.Tag,
			Format:       C.RuleSetFormatBinary,
			LocalOptions: O.LocalRuleSet{Path: path},
		}
		bundled = append(bundled, rs.Tag)
	}
	return bundled
}
