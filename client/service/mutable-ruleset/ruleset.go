/*
Package mutruleset provides constants for supported filters and ruleset types
*/
package mutruleset

import (
	"github.com/getlantern/sing-box-extensions/ruleset"
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
