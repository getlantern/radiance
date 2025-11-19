package vpn

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
)

const (
	adBlockTag = "adblock"
	// remote list (updated by sing-box)
	adBlockListTag = "adblock-list"
	adBlockFile    = adBlockTag + ".json"
)

// For headless ruleset JSON:
//
// {
//   "version": 3,
//   "rules": [
//     { "type": "logical", "logical": { "mode": "...", "rules": [...] } },
//     { "type": "rule_set", "rule_set": ["adblock-list"] }
//   ]
// }

// adblockHeadlessRule is a minimal wrapper around the structures we need.
// We reuse sing-box's LogicalHeadlessRule for the logical gate and use a string array
// for the rule set reference
type adblockHeadlessRule struct {
	Type    string                 `json:"type"`
	Logical *O.LogicalHeadlessRule `json:"logical,omitempty"`
	RuleSet []string               `json:"rule_set,omitempty"`
}

// adblockRuleSet is the top-level struct for persisting ad blocking rules
type adblockRuleSet struct {
	Version int                   `json:"version"`
	Rules   []adblockHeadlessRule `json:"rules"`
}

// AdBlocker tracks whether ad blocking is on and where its rules live
type AdBlocker struct {
	mode     string
	ruleFile string
	enabled  *atomic.Bool
}

// NewAdBlockerHandler wires ad blocking up to the data directory and loads
// or creates the rule file
func NewAdBlockerHandler() (*AdBlocker, error) {
	a := newAdBlocker(common.DataPath())
	if _, err := os.Stat(a.ruleFile); os.IsNotExist(err) {
		if err := a.save(); err != nil {
			return nil, fmt.Errorf("write adblock file: %w", err)
		}
	}
	if err := a.load(); err != nil {
		return nil, fmt.Errorf("load adblock file: %w", err)
	}
	return a, nil
}

func newAdBlocker(path string) *AdBlocker {
	return &AdBlocker{
		mode:     C.LogicalTypeAnd,
		ruleFile: filepath.Join(path, adBlockFile),
		enabled:  &atomic.Bool{},
	}
}

// IsEnabled checks if ad blocking is currently turned on
func (a *AdBlocker) IsEnabled() bool { return a.enabled.Load() }

// SetEnabled flips ad blocking on or off
func (a *AdBlocker) SetEnabled(enabled bool) error {
	prev := a.mode
	if enabled {
		a.mode = C.LogicalTypeOr
	} else {
		a.mode = C.LogicalTypeAnd
	}
	if err := a.save(); err != nil {
		a.mode = prev
		return err
	}
	a.enabled.Store(enabled)
	slog.Log(context.Background(), internal.LevelTrace, "updated adblock", "enabled", enabled)
	return nil
}

// save rewrites the adblock ruleset JSON on disk based on the current mode
func (a *AdBlocker) save() error {
	rs := adblockRuleSet{
		Version: 3,
		Rules: []adblockHeadlessRule{
			{
				Type: "logical",
				Logical: &O.LogicalHeadlessRule{
					Mode: a.mode,
					Rules: []O.HeadlessRule{
						{
							Type: "default",
							DefaultOptions: O.DefaultHeadlessRule{
								Domain: []string{"disable.rule"},
							},
						},
						{
							Type: "default",
							DefaultOptions: O.DefaultHeadlessRule{
								Domain: []string{"disable.rule"},
								Invert: true,
							},
						},
					},
				},
			},
			{
				Type:    "rule_set",
				RuleSet: []string{adBlockListTag},
			},
		},
	}

	buf, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("marshal adblock ruleset: %w", err)
	}
	return os.WriteFile(a.ruleFile, buf, 0o644)
}

// load reads the adblock ruleset from disk and updates the mode
func (a *AdBlocker) load() error {
	content, err := os.ReadFile(a.ruleFile)
	if err != nil {
		return fmt.Errorf("read adblock file: %w", err)
	}

	var rs adblockRuleSet
	if err := json.Unmarshal(content, &rs); err != nil {
		return fmt.Errorf("unmarshal adblock: %w", err)
	}

	if len(rs.Rules) == 0 || rs.Rules[0].Logical == nil {
		return fmt.Errorf("adblock file missing logical rule")
	}

	a.mode = rs.Rules[0].Logical.Mode
	a.enabled.Store(a.mode == C.LogicalTypeOr)
	return nil
}
