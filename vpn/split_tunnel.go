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
	"strings"
	"sync"
	"sync/atomic"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
)

const (
	splitTunnelTag  = "split-tunnel"
	splitTunnelFile = splitTunnelTag + ".json"

	TypeDomain           = "domain"
	TypeDomainSuffix     = "domainSuffix"
	TypeDomainKeyword    = "domainKeyword"
	TypeDomainRegex      = "domainRegex"
	TypeProcessName      = "processName"
	TypeProcessPath      = "processPath"
	TypeProcessPathRegex = "processPathRegex"
	TypePackageName      = "packageName"
)

// SplitTunnel manages the split tunneling feature, allowing users to specify which domains,
// processes, or packages should bypass the VPN tunnel.
type SplitTunnel struct {
	rule         O.LogicalHeadlessRule
	activeFilter *O.LogicalHeadlessRule
	ruleFile     string
	ruleMap      map[string]*O.DefaultHeadlessRule
	enabled      *atomic.Bool
	access       sync.Mutex
}

func NewSplitTunnelHandler() (*SplitTunnel, error) {
	s := newSplitTunnel(settings.GetString(settings.DataPathKey))
	if err := s.loadRule(); err != nil {
		return nil, fmt.Errorf("loading split tunnel rule file %s: %w", s.ruleFile, err)
	}
	return s, nil
}

func newSplitTunnel(path string) *SplitTunnel {
	rule := defaultRule()
	s := &SplitTunnel{
		rule:         rule,
		ruleFile:     filepath.Join(path, splitTunnelFile),
		activeFilter: &(rule.Rules[1].LogicalOptions),
		ruleMap:      make(map[string]*O.DefaultHeadlessRule),
		enabled:      &atomic.Bool{},
	}
	s.initRuleMap()
	if _, err := os.Stat(s.ruleFile); errors.Is(err, fs.ErrNotExist) {
		slog.Debug("Creating initial split tunnel rule file", "file", s.ruleFile)
		s.saveToFile()
	}
	return s
}

func (s *SplitTunnel) Enable() error {
	return s.setEnabled(true)
}

func (s *SplitTunnel) Disable() error {
	return s.setEnabled(false)
}

func (s *SplitTunnel) setEnabled(enabled bool) error {
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
	slog.Log(context.Background(), internal.LevelTrace, "Updated split-tunneling", "enabled", enabled)
	return nil
}

func (s *SplitTunnel) IsEnabled() bool {
	return s.enabled.Load()
}

func (s *SplitTunnel) Filters() Filter {
	s.access.Lock()
	defer s.access.Unlock()
	f := Filter{}
	if rule, ok := s.ruleMap["domain"]; ok {
		f.Domain = slices.Clone(rule.Domain)
		f.DomainSuffix = slices.Clone(rule.DomainSuffix)
		f.DomainKeyword = slices.Clone(rule.DomainKeyword)
		f.DomainRegex = slices.Clone(rule.DomainRegex)
	}
	if rule, ok := s.ruleMap["processName"]; ok {
		f.ProcessName = slices.Clone(rule.ProcessName)
	}
	if rule, ok := s.ruleMap["processPath"]; ok {
		f.ProcessPath = slices.Clone(rule.ProcessPath)
	}
	if rule, ok := s.ruleMap["processPathRegex"]; ok {
		f.ProcessPathRegex = slices.Clone(rule.ProcessPathRegex)
	}
	if rule, ok := s.ruleMap["packageName"]; ok {
		f.PackageName = slices.Clone(rule.PackageName)
	}
	return f
}

// AddItem adds a new item to the filter of the given type.
func (s *SplitTunnel) AddItem(filterType, item string) error {
	if err := s.updateFilter(filterType, item, merge); err != nil {
		return err
	}
	slog.Debug("added item to filter", "filterType", filterType, "item", item)
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
	slog.Debug("removed item from filter", "filterType", filterType, "item", item)
	if err := s.saveToFile(); err != nil {
		return fmt.Errorf("writing rule to %s: %w", s.ruleFile, err)
	}
	return nil
}

// AddItems adds multiple items to the filter.
func (s *SplitTunnel) AddItems(items Filter) error {
	s.updateFilters(items, merge)
	slog.Debug("added items to filter", "items", items.String())
	return s.saveToFile()
}

// RemoveItems removes multiple items from the filter.
func (s *SplitTunnel) RemoveItems(items Filter) error {
	s.updateFilters(items, remove)
	slog.Debug("removed items from filter", "items", items.String())
	return s.saveToFile()
}

type Filter struct {
	Domain           []string
	DomainSuffix     []string
	DomainKeyword    []string
	DomainRegex      []string
	ProcessName      []string
	ProcessPath      []string
	ProcessPathRegex []string
	PackageName      []string
}

func (f Filter) String() string {
	var str []string
	if len(f.Domain) > 0 {
		str = append(str, fmt.Sprintf("domain: %v", f.Domain))
	}
	if len(f.DomainSuffix) > 0 {
		str = append(str, fmt.Sprintf("domainSuffix: %v", f.DomainSuffix))
	}
	if len(f.DomainKeyword) > 0 {
		str = append(str, fmt.Sprintf("domainKeyword: %v", f.DomainKeyword))
	}
	if len(f.DomainRegex) > 0 {
		str = append(str, fmt.Sprintf("domainRegex: %v", f.DomainRegex))
	}
	if len(f.ProcessName) > 0 {
		str = append(str, fmt.Sprintf("processName: %v", f.ProcessName))
	}
	if len(f.ProcessPath) > 0 {
		str = append(str, fmt.Sprintf("processPath: %v", f.ProcessPath))
	}
	if len(f.ProcessPathRegex) > 0 {
		str = append(str, fmt.Sprintf("processPathRegex: %v", f.ProcessPathRegex))
	}
	if len(f.PackageName) > 0 {
		str = append(str, fmt.Sprintf("packageName: %v", f.PackageName))
	}
	return "{" + strings.Join(str, ", ") + "}"
}

type actionFn func(slice []string, items []string) []string

var validFilterTypes = []string{TypeDomain, TypeDomainSuffix, TypeDomainKeyword, TypeDomainRegex, TypePackageName, TypeProcessName, TypeProcessPath, TypeProcessPathRegex}

func (s *SplitTunnel) updateFilter(filterType string, item string, fn actionFn) error {
	s.access.Lock()
	defer s.access.Unlock()

	if !slices.Contains(validFilterTypes, filterType) {
		return fmt.Errorf("unsupported filter type: %s", filterType)
	}

	rule := s.ensureRuleExists(filterType)
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

func (s *SplitTunnel) updateFilters(diff Filter, fn actionFn) {
	s.access.Lock()
	defer s.access.Unlock()

	// Update domain rule
	if len(diff.Domain) > 0 || len(diff.DomainSuffix) > 0 ||
		len(diff.DomainKeyword) > 0 || len(diff.DomainRegex) > 0 {
		rule := s.ensureRuleExists("domain")
		if len(diff.Domain) > 0 {
			rule.Domain = fn(rule.Domain, diff.Domain)
		}
		if len(diff.DomainSuffix) > 0 {
			rule.DomainSuffix = fn(rule.DomainSuffix, diff.DomainSuffix)
		}
		if len(diff.DomainKeyword) > 0 {
			rule.DomainKeyword = fn(rule.DomainKeyword, diff.DomainKeyword)
		}
		if len(diff.DomainRegex) > 0 {
			rule.DomainRegex = fn(rule.DomainRegex, diff.DomainRegex)
		}
	}

	// Update processName rule
	if len(diff.ProcessName) > 0 {
		rule := s.ensureRuleExists("processName")
		rule.ProcessName = fn(rule.ProcessName, diff.ProcessName)
	}

	// Update processPath rule
	if len(diff.ProcessPath) > 0 {
		rule := s.ensureRuleExists("processPath")
		rule.ProcessPath = fn(rule.ProcessPath, diff.ProcessPath)
	}

	// Update processPathRegex rule
	if len(diff.ProcessPathRegex) > 0 {
		rule := s.ensureRuleExists("processPathRegex")
		rule.ProcessPathRegex = fn(rule.ProcessPathRegex, diff.ProcessPathRegex)
	}

	// Update packageName rule
	if len(diff.PackageName) > 0 {
		rule := s.ensureRuleExists("packageName")
		rule.PackageName = fn(rule.PackageName, diff.PackageName)
	}
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
	rule := s.rule
	rule.Rules[1].LogicalOptions.Rules = slices.DeleteFunc(rule.Rules[1].LogicalOptions.Rules, func(r O.HeadlessRule) bool {
		return isEmptyRule(r.DefaultOptions)
	})

	if len(rule.Rules[1].LogicalOptions.Rules) == 0 {
		rule.Rules = rule.Rules[:1] // remove the default rule if it's empty
	}
	rs := O.PlainRuleSetCompat{
		Version: 3,
		Options: O.PlainRuleSet{
			Rules: []O.HeadlessRule{
				{
					Type:           "logical",
					LogicalOptions: rule,
				},
			},
		},
	}
	buf, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("marshalling rule set: %w", err)
	}
	if err := atomicfile.WriteFile(s.ruleFile, buf, 0644); err != nil {
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
	content, err := atomicfile.ReadFile(s.ruleFile)
	// the file should exist at this point, so we don't need to check for fs.ErrNotExist
	if err != nil {
		return fmt.Errorf("reading rule file %s: %w", s.ruleFile, err)
	}
	ruleSet, err := json.UnmarshalExtended[O.PlainRuleSetCompat](content)
	if err != nil {
		return fmt.Errorf("unmarshalling rule file %s: %w", s.ruleFile, err)
	}
	rules := ruleSet.Options.Rules
	if len(rules) == 0 {
		slog.Warn("split tunnel rule file format is invalid, using empty rule")
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
		slog.Debug("Migrating legacy split tunnel rule format")
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

	slog.Log(context.Background(), internal.LevelTrace, "loaded split tunnel rules",
		"file", s.ruleFile, "filters", s.Filters().String(), "enabled", s.IsEnabled(),
	)
	return nil
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

	for i := range s.activeFilter.Rules {
		rule := &s.activeFilter.Rules[i].DefaultOptions

		// Categorize the rule based on its contents
		if len(rule.Domain) > 0 || len(rule.DomainSuffix) > 0 ||
			len(rule.DomainKeyword) > 0 || len(rule.DomainRegex) > 0 {
			s.ruleMap["domain"] = rule
		}
		if len(rule.ProcessName) > 0 {
			s.ruleMap["processName"] = rule
		}
		if len(rule.ProcessPath) > 0 {
			s.ruleMap["processPath"] = rule
		}
		if len(rule.ProcessPathRegex) > 0 {
			s.ruleMap["processPathRegex"] = rule
		}
		if len(rule.PackageName) > 0 {
			s.ruleMap["packageName"] = rule
		}
	}
}

func (s *SplitTunnel) ensureRuleExists(category string) *O.DefaultHeadlessRule {
	switch category {
	case TypeDomainKeyword, TypeDomainRegex, TypeDomainSuffix:
		category = TypeDomain
	}
	if rule, ok := s.ruleMap[category]; ok {
		return rule
	}

	// Create new rule and add it to activeFilter
	s.activeFilter.Rules = append(s.activeFilter.Rules, O.HeadlessRule{
		Type:           C.RuleTypeDefault,
		DefaultOptions: O.DefaultHeadlessRule{},
	})
	s.ruleMap[category] = &s.activeFilter.Rules[len(s.activeFilter.Rules)-1].DefaultOptions
	return s.ruleMap[category]
}
