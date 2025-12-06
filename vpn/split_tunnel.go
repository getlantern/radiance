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

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
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
	activeFilter *O.DefaultHeadlessRule
	ruleFile     string

	enabled *atomic.Bool
	access  sync.Mutex
}

func NewSplitTunnelHandler() (*SplitTunnel, error) {
	s := newSplitTunnel(common.DataPath())
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
		activeFilter: &(rule.Rules[1].DefaultOptions),
		enabled:      &atomic.Bool{},
	}
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
	return Filter{
		Domain:           slices.Clone(s.activeFilter.Domain),
		DomainSuffix:     slices.Clone(s.activeFilter.DomainSuffix),
		DomainKeyword:    slices.Clone(s.activeFilter.DomainKeyword),
		DomainRegex:      slices.Clone(s.activeFilter.DomainRegex),
		ProcessName:      slices.Clone(s.activeFilter.ProcessName),
		ProcessPath:      slices.Clone(s.activeFilter.ProcessPath),
		ProcessPathRegex: slices.Clone(s.activeFilter.ProcessPathRegex),
		PackageName:      slices.Clone(s.activeFilter.PackageName),
	}
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

func (s *SplitTunnel) updateFilter(filterType string, item string, fn actionFn) error {
	s.access.Lock()
	defer s.access.Unlock()

	items := []string{item}
	switch filterType {
	case TypeDomain:
		s.activeFilter.Domain = fn(s.activeFilter.Domain, items)
	case TypeDomainSuffix:
		s.activeFilter.DomainSuffix = fn(s.activeFilter.DomainSuffix, items)
	case TypeDomainKeyword:
		s.activeFilter.DomainKeyword = fn(s.activeFilter.DomainKeyword, items)
	case TypeDomainRegex:
		s.activeFilter.DomainRegex = fn(s.activeFilter.DomainRegex, items)
	case TypeProcessName:
		s.activeFilter.ProcessName = fn(s.activeFilter.ProcessName, items)
	case TypeProcessPath:
		s.activeFilter.ProcessPath = fn(s.activeFilter.ProcessPath, items)
	case TypeProcessPathRegex:
		s.activeFilter.ProcessPathRegex = fn(s.activeFilter.ProcessPathRegex, items)
	case TypePackageName:
		s.activeFilter.PackageName = fn(s.activeFilter.PackageName, items)
	default:
		return fmt.Errorf("unsupported filter type: %s", filterType)
	}
	return nil
}

func (s *SplitTunnel) updateFilters(diff Filter, fn actionFn) {
	s.access.Lock()
	defer s.access.Unlock()
	if len(diff.Domain) > 0 {
		s.activeFilter.Domain = fn(s.activeFilter.Domain, diff.Domain)
	}
	if len(diff.DomainSuffix) > 0 {
		s.activeFilter.DomainSuffix = fn(s.activeFilter.DomainSuffix, diff.DomainSuffix)
	}
	if len(diff.DomainKeyword) > 0 {
		s.activeFilter.DomainKeyword = fn(s.activeFilter.DomainKeyword, diff.DomainKeyword)
	}
	if len(diff.DomainRegex) > 0 {
		s.activeFilter.DomainRegex = fn(s.activeFilter.DomainRegex, diff.DomainRegex)
	}
	if len(diff.ProcessName) > 0 {
		s.activeFilter.ProcessName = fn(s.activeFilter.ProcessName, diff.ProcessName)
	}
	if len(diff.ProcessPath) > 0 {
		s.activeFilter.ProcessPath = fn(s.activeFilter.ProcessPath, diff.ProcessPath)
	}
	if len(diff.ProcessPathRegex) > 0 {
		s.activeFilter.ProcessPathRegex = fn(s.activeFilter.ProcessPathRegex, diff.ProcessPathRegex)
	}
	if len(diff.PackageName) > 0 {
		s.activeFilter.PackageName = fn(s.activeFilter.PackageName, diff.PackageName)
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
	if isEmptyRule(rule.Rules[1].DefaultOptions) {
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
			Type:           C.RuleTypeDefault,
			DefaultOptions: O.DefaultHeadlessRule{},
		})
	}
	s.activeFilter = &(s.rule.Rules[1].DefaultOptions)
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
				Type:           C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{},
			},
		},
	}
}
