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
// LOCAL rule-sets backed by the .srs bundled above, eliminating the cold-start
// network fetch. There is no GFW-reachable host to fetch it from at our scale —
// S3 is rate-throttled, jsDelivr org-blocks getlantern, raw.githubusercontent is
// flaky + scale-risky — so the bundled copy is the reliable floor: the fetch
// can't fail (it doesn't happen) and the rule-set always loads. A later fronted-
// refresh layer can overwrite the on-disk copy to keep it current between
// releases. Returns the tags that were bundled.
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
		// Always (re)write: the bundled copy is the source of truth until a
		// fronted-refresh layer exists to drop a fresher copy at this path.
		if err := atomicfile.WriteFile(path, bundledGeositeCN, fileperm.File); err != nil {
			slog.Warn("bundle geosite: write failed; leaving remote rule-set",
				slog.String("tag", rs.Tag), slog.Any("error", err))
			continue
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
