package mutruleset

import (
	"errors"
	"path/filepath"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

const (
	TypeDomain        = "domain"        // ex: "example.com"
	TypeDomainSuffix  = "domainSuffix"  // ex: ".cn"
	TypeDomainKeyword = "domainKeyword" // ex: "example"
	TypeDomainRegex   = "domainRegex"   // ex: ".*\.com"
	TypeProcessName   = "processName"   // ex: "chrome"
	TypePackageName   = "packageName"   // ex: "com.android.chrome" (Android)
)

type RuleSet = option.DefaultHeadlessRule

// FilePath constructs a file path for a ruleset with the given tag. The file path will have the
// appropriate extension based on the given format.
func FilePath(datadir, tag, format string) (string, error) {
	switch format {
	case constant.RuleSetFormatSource, "":
		return filepath.Join(datadir, tag+".json"), nil
	case constant.RuleSetFormatBinary:
		return filepath.Join(datadir, tag+".srs"), nil
	}
	return "", errors.New("unsupported format")
}
