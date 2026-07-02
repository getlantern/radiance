//go:build !novpn

package vpn

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
)

const splitTunnelFile = internal.SplitTunnelFileName

// SplitTunnel manages the split tunneling feature, allowing users to specify which domains,
// processes, or packages should bypass the VPN tunnel.
type SplitTunnel struct {
	rule         O.LogicalHeadlessRule
	activeFilter *O.LogicalHeadlessRule
	ruleFile     string
	ruleMap      map[string]*O.DefaultHeadlessRule
	enabled      *atomic.Bool
	access       sync.Mutex
	logger       *slog.Logger
}

// NewSplitTunnelHandler creates a SplitTunnel handler, loading the saved rule
// set from disk.
//
// The returned handler is always usable, even when the error is non-nil: an
// unreadable or unparseable rule file — e.g. one this build's sing-box can't
// decode after a version downgrade — falls back to the default no-op rule,
// yielding a working handler plus an error describing the failure.
func NewSplitTunnelHandler(dataPath string, logger *slog.Logger) (*SplitTunnel, error) {
	s := newSplitTunnel(dataPath, logger)
	if err := s.loadRule(); err != nil {
		return s, fmt.Errorf("loading split tunnel rule file %s: %w", s.ruleFile, err)
	}
	return s, nil
}

func newSplitTunnel(path string, logger *slog.Logger) *SplitTunnel {
	rule := defaultRule()
	s := &SplitTunnel{
		rule:         rule,
		ruleFile:     filepath.Join(path, splitTunnelFile),
		activeFilter: &(rule.Rules[1].LogicalOptions),
		ruleMap:      make(map[string]*O.DefaultHeadlessRule),
		enabled:      &atomic.Bool{},
		logger:       logger,
	}
	s.initRuleMap()
	if _, err := os.Stat(s.ruleFile); errors.Is(err, fs.ErrNotExist) {
		logger.Debug("Creating initial split tunnel rule file", "file", s.ruleFile)
		s.saveToFile()
	}
	return s
}

func (s *SplitTunnel) SetEnabled(enabled bool) error {
	if s.enabled.Load() == enabled {
		return nil
	}
	mode := C.LogicalTypeAnd
	if enabled {
		mode = C.LogicalTypeOr
	}
	prev := s.rule.Mode
	s.rule.Mode = mode
	if err := s.saveToFile(); err != nil {
		s.rule.Mode = prev
		return fmt.Errorf("writing rule to %s: %w", s.ruleFile, err)
	}
	s.enabled.Store(enabled)
	s.logger.Log(context.Background(), log.LevelTrace, "Updated split-tunneling", "enabled", enabled)
	return nil
}

func (s *SplitTunnel) IsEnabled() bool {
	return s.enabled.Load()
}

func (s *SplitTunnel) Filters() SplitTunnelFilter {
	s.access.Lock()
	defer s.access.Unlock()
	return SplitTunnelFilter{
		Domain:           slices.Clone(s.ruleMap[TypeDomain].Domain),
		DomainSuffix:     slices.Clone(s.ruleMap[TypeDomainSuffix].DomainSuffix),
		DomainKeyword:    slices.Clone(s.ruleMap[TypeDomainKeyword].DomainKeyword),
		DomainRegex:      slices.Clone(s.ruleMap[TypeDomainRegex].DomainRegex),
		ProcessName:      slices.Clone(s.ruleMap[TypeProcessName].ProcessName),
		ProcessPath:      slices.Clone(s.ruleMap[TypeProcessPath].ProcessPath),
		ProcessPathRegex: slices.Clone(s.ruleMap[TypeProcessPathRegex].ProcessPathRegex),
		PackageName:      slices.Clone(s.ruleMap[TypePackageName].PackageName),
	}
}

// AddItem adds a new item to the filter of the given type.
func (s *SplitTunnel) AddItem(filterType, item string) error {
	if err := s.updateFilter(filterType, item, merge); err != nil {
		return err
	}
	s.logger.Debug("added item to filter", "filterType", filterType, "item", item)
	if err := s.saveToFile(); err != nil {
		return fmt.Errorf("writing rule to %s: %w", s.ruleFile, err)
	}
	return nil
}

// RemoveItem removes an item from the filter of the given type.
func (s *SplitTunnel) RemoveItem(filterType, item string) error {
	if err := s.updateFilter(filterType, item, remove); err != nil {
		return err
	}
	s.logger.Debug("removed item from filter", "filterType", filterType, "item", item)
	if err := s.saveToFile(); err != nil {
		return fmt.Errorf("writing rule to %s: %w", s.ruleFile, err)
	}
	return nil
}

// AddItems adds multiple items to the filter.
func (s *SplitTunnel) AddItems(items SplitTunnelFilter) error {
	s.updateFilters(items, merge)
	s.logger.Debug("added items to filter", "items", items.String())
	return s.saveToFile()
}

// RemoveItems removes multiple items from the filter.
func (s *SplitTunnel) RemoveItems(items SplitTunnelFilter) error {
	s.updateFilters(items, remove)
	s.logger.Debug("removed items from filter", "items", items.String())
	return s.saveToFile()
}

type actionFn func(slice []string, items []string) []string

func (s *SplitTunnel) updateFilter(filterType string, item string, fn actionFn) error {
	s.access.Lock()
	defer s.access.Unlock()
	rule, exist := s.ruleMap[filterType]
	if !exist {
		return fmt.Errorf("unsupported filter type: %s", filterType)
	}

	items := []string{item}
	switch filterType {
	case TypeDomain:
		rule.Domain = fn(rule.Domain, items)
	case TypeDomainSuffix:
		rule.DomainSuffix = fn(rule.DomainSuffix, items)
	case TypeDomainKeyword:
		rule.DomainKeyword = fn(rule.DomainKeyword, items)
	case TypeDomainRegex:
		rule.DomainRegex = fn(rule.DomainRegex, items)
	case TypeProcessName:
		rule.ProcessName = fn(rule.ProcessName, items)
	case TypeProcessPath:
		rule.ProcessPath = fn(rule.ProcessPath, items)
	case TypeProcessPathRegex:
		rule.ProcessPathRegex = fn(rule.ProcessPathRegex, items)
	case TypePackageName:
		rule.PackageName = fn(rule.PackageName, items)
	}
	return nil
}

func (s *SplitTunnel) updateFilters(diff SplitTunnelFilter, fn actionFn) {
	s.access.Lock()
	defer s.access.Unlock()

	s.ruleMap[TypeDomain].Domain = fn(s.ruleMap[TypeDomain].Domain, diff.Domain)
	s.ruleMap[TypeDomainSuffix].DomainSuffix = fn(s.ruleMap[TypeDomainSuffix].DomainSuffix, diff.DomainSuffix)
	s.ruleMap[TypeDomainKeyword].DomainKeyword = fn(s.ruleMap[TypeDomainKeyword].DomainKeyword, diff.DomainKeyword)
	s.ruleMap[TypeDomainRegex].DomainRegex = fn(s.ruleMap[TypeDomainRegex].DomainRegex, diff.DomainRegex)
	s.ruleMap[TypeProcessName].ProcessName = fn(s.ruleMap[TypeProcessName].ProcessName, diff.ProcessName)
	s.ruleMap[TypeProcessPath].ProcessPath = fn(s.ruleMap[TypeProcessPath].ProcessPath, diff.ProcessPath)
	s.ruleMap[TypeProcessPathRegex].ProcessPathRegex = fn(s.ruleMap[TypeProcessPathRegex].ProcessPathRegex, diff.ProcessPathRegex)
	s.ruleMap[TypePackageName].PackageName = fn(s.ruleMap[TypePackageName].PackageName, diff.PackageName)
}

func merge(slice []string, items []string) []string {
	return append(slice, items...)
}

// remove removes all items in items from s.
func remove(s []string, items []string) []string {
	i := slices.IndexFunc(s, func(v string) bool {
		return slices.Contains(items, v)
	})
	if i == -1 {
		return s // no items to remove
	}
	for j := i + 1; j < len(s); j++ {
		if v := s[j]; !slices.Contains(items, v) {
			s[i] = v
			i++
		}
	}

	clear(s[i:])
	return s[:i]
}

func (s *SplitTunnel) saveToFile() error {
	// Build a serialization-only copy of the rules, filtering out empty entries
	// without mutating the live activeFilter/ruleMap state.
	filterRules := make([]O.HeadlessRule, 0, len(s.activeFilter.Rules))
	for _, r := range s.activeFilter.Rules {
		if !isEmptyRule(r.DefaultOptions) {
			filterRules = append(filterRules, r)
		}
	}

	outerRules := []O.HeadlessRule{s.rule.Rules[0]} // disable rule
	if len(filterRules) > 0 {
		outerRules = append(outerRules, O.HeadlessRule{
			Type: s.rule.Rules[1].Type,
			LogicalOptions: O.LogicalHeadlessRule{
				Mode:  s.activeFilter.Mode,
				Rules: filterRules,
			},
		})
	}

	rs := O.PlainRuleSetCompat{
		Version: 3,
		Options: O.PlainRuleSet{
			Rules: []O.HeadlessRule{
				{
					Type: "logical",
					LogicalOptions: O.LogicalHeadlessRule{
						Mode:  s.rule.Mode,
						Rules: outerRules,
					},
				},
			},
		},
	}
	buf, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("marshalling rule set: %w", err)
	}
	if err := atomicfile.WriteFile(s.ruleFile, buf, fileperm.File); err != nil {
		return fmt.Errorf("writing rule file %s: %w", s.ruleFile, err)
	}
	return nil
}

func isEmptyRule(rule O.DefaultHeadlessRule) bool {
	return len(rule.Domain) == 0 && len(rule.DomainSuffix) == 0 &&
		len(rule.DomainKeyword) == 0 && len(rule.DomainRegex) == 0 &&
		len(rule.ProcessName) == 0 && len(rule.PackageName) == 0 &&
		len(rule.ProcessPath) == 0 && len(rule.ProcessPathRegex) == 0
}

func (s *SplitTunnel) loadRule() error {
	rawRuleSet, err := atomicfile.ReadFile(s.ruleFile)
	// the file should exist at this point, so we don't need to check for fs.ErrNotExist
	if err != nil {
		return fmt.Errorf("reading rule file %s: %w", s.ruleFile, err)
	}
	ruleSet, err := json.UnmarshalExtended[O.PlainRuleSetCompat](rawRuleSet)
	if err != nil {
		s.quarantineInvalidRuleSet(rawRuleSet)
		return fmt.Errorf("unmarshalling rule file %s: %w", s.ruleFile, err)
	}
	rules := ruleSet.Options.Rules
	if len(rules) == 0 {
		s.logger.Warn("split tunnel rule file format is invalid, using empty rule")
		return nil
	}

	s.rule = rules[0].LogicalOptions
	if len(s.rule.Rules) == 1 {
		s.rule.Rules = append(s.rule.Rules, O.HeadlessRule{
			Type: C.RuleTypeLogical,
			LogicalOptions: O.LogicalHeadlessRule{
				Mode:  C.LogicalTypeOr,
				Rules: []O.HeadlessRule{},
			},
		})
	} else if len(s.rule.Rules) > 1 && s.rule.Rules[1].Type == C.RuleTypeDefault {
		// Migrate legacy format: wrap DefaultOptions into LogicalOptions
		// TODO(2/10): remove in future commit
		s.logger.Debug("Migrating legacy split tunnel rule format")
		legacyRule := s.rule.Rules[1].DefaultOptions
		s.rule.Rules[1] = O.HeadlessRule{
			Type: C.RuleTypeLogical,
			LogicalOptions: O.LogicalHeadlessRule{
				Mode:  C.LogicalTypeOr,
				Rules: []O.HeadlessRule{},
			},
		}
		if len(legacyRule.Domain) > 0 ||
			len(legacyRule.DomainSuffix) > 0 ||
			len(legacyRule.DomainKeyword) > 0 ||
			len(legacyRule.DomainRegex) > 0 {
			s.rule.Rules[1].LogicalOptions.Rules = append(s.rule.Rules[1].LogicalOptions.Rules, O.HeadlessRule{
				Type: C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{
					Domain:        legacyRule.Domain,
					DomainSuffix:  legacyRule.DomainSuffix,
					DomainKeyword: legacyRule.DomainKeyword,
					DomainRegex:   legacyRule.DomainRegex,
				},
			})
		}
		if len(legacyRule.PackageName) > 0 {
			s.rule.Rules[1].LogicalOptions.Rules = append(s.rule.Rules[1].LogicalOptions.Rules, O.HeadlessRule{
				Type: C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{
					PackageName: legacyRule.PackageName,
				},
			})
		}
		if len(legacyRule.ProcessName) > 0 {
			s.rule.Rules[1].LogicalOptions.Rules = append(s.rule.Rules[1].LogicalOptions.Rules, O.HeadlessRule{
				Type: C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{
					ProcessName: legacyRule.ProcessName,
				},
			})
		}
		if len(legacyRule.ProcessPath) > 0 {
			s.rule.Rules[1].LogicalOptions.Rules = append(s.rule.Rules[1].LogicalOptions.Rules, O.HeadlessRule{
				Type: C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{
					ProcessPath: legacyRule.ProcessPath,
				},
			})
		}
		if len(legacyRule.ProcessPathRegex) > 0 {
			s.rule.Rules[1].LogicalOptions.Rules = append(s.rule.Rules[1].LogicalOptions.Rules, O.HeadlessRule{
				Type: C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{
					ProcessPathRegex: legacyRule.ProcessPathRegex,
				},
			})
		}
	}
	s.activeFilter = &(s.rule.Rules[1].LogicalOptions)
	s.initRuleMap()
	s.enabled.Store(s.rule.Mode == C.LogicalTypeOr)

	s.logger.Log(context.Background(), log.LevelTrace, "loaded split tunnel rules",
		"file", s.ruleFile, "filters", s.Filters().String(), "enabled", s.IsEnabled(),
	)
	return nil
}

// quarantineInvalidRuleSet copies the unparseable rule file aside to
// split-tunnel.invalid.json for diagnostics. The original is left in place so a
// later re-upgrade can still read rules this build couldn't.
func (s *SplitTunnel) quarantineInvalidRuleSet(content []byte) {
	invalidPath := filepath.Join(filepath.Dir(s.ruleFile), internal.SplitTunnelInvalidFileName)
	if err := atomicfile.WriteFile(invalidPath, content, fileperm.File); err != nil {
		s.logger.Error("Writing invalid split tunnel copy", "path", invalidPath, "error", err)
		return
	}
	s.logger.Warn("Preserved unparseable split tunnel file for diagnostics", "path", invalidPath)
}

func defaultRule() O.LogicalHeadlessRule {
	// We use the logical rule type, here, like a logic gate. The first rule (inner logical rule)
	// acts as the "disable" state and the second is the actual filter. The "disable" rule basically
	// equates to "isEqual && !isEqual", which is always false. Switching the outter logical rule
	// mode between "or" and "and", effectively enables or disables split tunneling, respectively.
	return O.LogicalHeadlessRule{
		Mode: C.LogicalTypeAnd,
		Rules: []O.HeadlessRule{
			{
				Type: C.RuleTypeLogical,
				LogicalOptions: O.LogicalHeadlessRule{
					Mode: C.LogicalTypeAnd,
					Rules: []O.HeadlessRule{
						{
							Type:           C.RuleTypeDefault,
							DefaultOptions: O.DefaultHeadlessRule{Domain: []string{"disable.rule"}},
						},
						{
							Type:           C.RuleTypeDefault,
							DefaultOptions: O.DefaultHeadlessRule{Domain: []string{"disable.rule"}, Invert: true},
						},
					},
				},
			},
			{
				Type: C.RuleTypeLogical,
				LogicalOptions: O.LogicalHeadlessRule{
					Mode:  C.LogicalTypeOr,
					Rules: []O.HeadlessRule{},
				},
			},
		},
	}
}

func (s *SplitTunnel) initRuleMap() {
	s.ruleMap = make(map[string]*O.DefaultHeadlessRule)

	categories := []string{TypeDomain, TypeProcessName, TypeProcessPath, TypeProcessPathRegex, TypePackageName}

	// First pass: find which categories already have rules, and ensure empty
	// rules exist for the rest. All appends happen before any pointers are
	// stored so that slice reallocation cannot invalidate them.
	found := make(map[string]bool, len(categories))
	for i := range s.activeFilter.Rules {
		rule := &s.activeFilter.Rules[i].DefaultOptions
		if len(rule.Domain) > 0 || len(rule.DomainSuffix) > 0 ||
			len(rule.DomainKeyword) > 0 || len(rule.DomainRegex) > 0 {
			found[TypeDomain] = true
		}
		if len(rule.ProcessName) > 0 {
			found[TypeProcessName] = true
		}
		if len(rule.ProcessPath) > 0 {
			found[TypeProcessPath] = true
		}
		if len(rule.ProcessPathRegex) > 0 {
			found[TypeProcessPathRegex] = true
		}
		if len(rule.PackageName) > 0 {
			found[TypePackageName] = true
		}
	}
	for _, cat := range categories {
		if !found[cat] {
			s.activeFilter.Rules = append(s.activeFilter.Rules, O.HeadlessRule{
				Type:           C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{},
			})
		}
	}

	// Second pass: the slice is now stable — store pointers into ruleMap.
	// Empty rules are assigned to the first unmatched category.
	emptyIdx := 0
	missing := make([]string, 0, len(categories))
	for _, cat := range categories {
		if !found[cat] {
			missing = append(missing, cat)
		}
	}
	for i := range s.activeFilter.Rules {
		rule := &s.activeFilter.Rules[i].DefaultOptions
		matched := false
		if len(rule.Domain) > 0 || len(rule.DomainSuffix) > 0 ||
			len(rule.DomainKeyword) > 0 || len(rule.DomainRegex) > 0 {
			s.ruleMap[TypeDomain] = rule
			matched = true
		}
		if len(rule.ProcessName) > 0 {
			s.ruleMap[TypeProcessName] = rule
			matched = true
		}
		if len(rule.ProcessPath) > 0 {
			s.ruleMap[TypeProcessPath] = rule
			matched = true
		}
		if len(rule.ProcessPathRegex) > 0 {
			s.ruleMap[TypeProcessPathRegex] = rule
			matched = true
		}
		if len(rule.PackageName) > 0 {
			s.ruleMap[TypePackageName] = rule
			matched = true
		}
		if !matched && emptyIdx < len(missing) {
			s.ruleMap[missing[emptyIdx]] = rule
			emptyIdx++
		}
	}

	s.ruleMap[TypeDomainKeyword] = s.ruleMap[TypeDomain]
	s.ruleMap[TypeDomainRegex] = s.ruleMap[TypeDomain]
	s.ruleMap[TypeDomainSuffix] = s.ruleMap[TypeDomain]
}
