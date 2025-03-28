/*
Package mutruleset provides constants for supported filter types and utility functions for working
with rulesets.
*/
package mutruleset

import (
	"errors"
	"path/filepath"

	"github.com/getlantern/sing-box-extensions/ruleset"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

const (
	TypeDomain        = ruleset.TypeDomain        // ex: "example.com"
	TypeDomainSuffix  = ruleset.TypeDomainSuffix  // ex: ".cn"
	TypeDomainKeyword = ruleset.TypeDomainKeyword // ex: "example"
	TypeDomainRegex   = ruleset.TypeDomainRegex   // ex: ".*\.com"
	TypeProcessName   = ruleset.TypeProcessName   // ex: "chrome"
	TypePackageName   = ruleset.TypePackageName   // ex: "com.android.chrome" (Android)
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
