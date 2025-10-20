package vpn

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/getlantern/radiance/common"
)

const (
	adBlockTag  = "ad-block"
	adBlockFile = adBlockTag + ".json"
)

const emptyRuleSetJSON = `{"version":3,"rules":[]}`

const defaultAdBlockRuleSet = `
{
  "version": 3,
  "rules": [
    { "domain_suffix": [
      "doubleclick.net",
      "googlesyndication.com",
      "googletagservices.com",
      "adservice.google.com",
      "adnxs.com",
      "ads.yahoo.com"
    ]}
  ]
}`

type AdBlocker struct{ ruleFile string }

func NewAdBlockerHandler() (*AdBlocker, error) {
	path := filepath.Join(common.DataPath(), adBlockFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_ = os.WriteFile(path, []byte(emptyRuleSetJSON), 0644)
	}
	return &AdBlocker{ruleFile: path}, nil
}

func (a *AdBlocker) SetEnabled(enabled bool) error {
	if enabled {
		return os.WriteFile(a.ruleFile, []byte(defaultAdBlockRuleSet), 0644)
	}
	return os.WriteFile(a.ruleFile, []byte(emptyRuleSetJSON), 0644)
}

func (a *AdBlocker) IsEnabled() bool {
	data, err := os.ReadFile(a.ruleFile)
	if err != nil {
		return false
	}
	return !bytes.Contains(data, []byte(`"rules":[]`))
}
