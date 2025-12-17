// file: vpn/adblocker.go
package vpn

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/internal"
)

const (
	adBlockTag = "adblock"
	// remote list (updated by sing-box)
	adBlockListTag = "adblock-list"
	adBlockFile    = adBlockTag + ".json"
)

// adblockHeadlessRule is a minimal wrapper for ad blocking around O.LogicalHeadlessRule
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

	enabled atomic.Bool
	access  sync.Mutex
}

// NewAdBlocker creates a new instance of ad blocker, wired to the data directory
// and loads (or creates) the adblock rule file
func NewAdBlocker() (*AdBlocker, error) {
	a := newAdBlocker(common.DataPath())

	// Create parent dir if needed (defensive for early startup paths)
	if err := os.MkdirAll(filepath.Dir(a.ruleFile), 0o755); err != nil {
		return nil, fmt.Errorf("create adblock dir: %w", err)
	}

	if _, err := os.Stat(a.ruleFile); errors.Is(err, fs.ErrNotExist) {
		if err := a.save(); err != nil {
			return nil, fmt.Errorf("write adblock file: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat adblock file: %w", err)
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
	}
}

// createAdBlockRuleFile creates the adblock rules file if it does not exist
func createAdBlockRuleFile(basePath string) error {
	if basePath == "" {
		return fmt.Errorf("basePath is empty")
	}
	if err := os.MkdirAll(basePath, 0o755); err != nil {
		return fmt.Errorf("create basePath: %w", err)
	}

	a := newAdBlocker(basePath)

	_, err := os.Stat(a.ruleFile)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		if err := a.save(); err != nil {
			slog.Warn("Failed to save default adblock rule set", "path", a.ruleFile, "error", err)
			return err
		}
		return nil
	default:
		slog.Warn("Failed to stat adblock rule set", "path", a.ruleFile, "error", err)
		return err
	}
}

// IsEnabled returns whether or not ad blocking is currently on.
func (a *AdBlocker) IsEnabled() bool { return a.enabled.Load() }

// SetEnabled flips ad blocking on or off.
func (a *AdBlocker) SetEnabled(enabled bool) error {
	a.access.Lock()
	defer a.access.Unlock()

	if a.enabled.Load() == enabled {
		return nil
	}

	prevMode := a.mode
	if enabled {
		a.mode = C.LogicalTypeOr
	} else {
		a.mode = C.LogicalTypeAnd
	}

	if err := a.saveLocked(); err != nil {
		a.mode = prevMode
		return err
	}

	a.enabled.Store(enabled)
	slog.Log(context.Background(), internal.LevelTrace, "updated adblock", "enabled", enabled)
	return nil
}

// save updates the current mode in the adblock ruleset JSON and saves it to disk.
func (a *AdBlocker) save() error {
	a.access.Lock()
	defer a.access.Unlock()
	return a.saveLocked()
}

// saveLocked assumes a.access is already held.
func (a *AdBlocker) saveLocked() error {
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

	if err := atomicfile.WriteFile(a.ruleFile, buf, 0o644); err != nil {
		return fmt.Errorf("write adblock file: %w", err)
	}
	return nil
}

// load reads the adblock ruleset from disk and updates the mode
func (a *AdBlocker) load() error {
	a.access.Lock()
	defer a.access.Unlock()
	return a.loadLocked()
}

func (a *AdBlocker) loadLocked() error {
	content, err := atomicfile.ReadFile(a.ruleFile)
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
