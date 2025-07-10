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

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
)

const (
	splitTunnelTag  = "split-tunnel"
	splitTunnelFile = splitTunnelTag + ".json"

	TypeDomain        = "domain"
	TypeDomainSuffix  = "domainSuffix"
	TypeDomainKeyword = "domainKeyword"
	TypeDomainRegex   = "domainRegex"
	TypeProcessName   = "processName"
	TypePackageName   = "packageName"
)

// SplitTunnel manages the split tunneling feature, allowing users to specify which domains,
// processes, or packages should bypass the VPN tunnel.
type SplitTunnel struct {
	rule         O.LogicalHeadlessRule
	activeFilter *O.DefaultHeadlessRule
	ruleFile     string

	log *slog.Logger

	access sync.Mutex
}

func NewSplitTunnelHandler() (*SplitTunnel, error) {
	ruleFile := filepath.Join(common.DataPath(), splitTunnelFile)
	rule := defaultRule()
	s := &SplitTunnel{
		rule:         rule,
		ruleFile:     ruleFile,
		activeFilter: &(rule.Rules[0].DefaultOptions),
		log:          slog.Default().With("service", "split-tunnel"),
	}
	if err := s.loadRule(); err != nil {
		return nil, fmt.Errorf("loading split tunnel rule file %s: %w", ruleFile, err)
	}
	return s, nil
}

func (s *SplitTunnel) Enable() {
	if s.IsEnabled() {
		return
	}
	s.access.Lock()
	s.rule.Mode = C.LogicalTypeOr
	s.access.Unlock()
	s.saveToFile()
	s.log.Log(context.Background(), internal.LevelTrace, "enabled split tunneling")
}

func (s *SplitTunnel) Disable() {
	if !s.IsEnabled() {
		return
	}
	s.access.Lock()
	s.rule.Mode = C.LogicalTypeAnd
	s.access.Unlock()
	s.saveToFile()
	s.log.Log(context.Background(), internal.LevelTrace, "disabled split tunneling")
}

func (s *SplitTunnel) IsEnabled() bool {
	s.access.Lock()
	defer s.access.Unlock()
	return s.rule.Mode == C.LogicalTypeOr
}

func (s *SplitTunnel) Filters() Filter {
	s.access.Lock()
	defer s.access.Unlock()
	return Filter{
		Domain:        slices.Clone(s.activeFilter.Domain),
		DomainSuffix:  slices.Clone(s.activeFilter.DomainSuffix),
		DomainKeyword: slices.Clone(s.activeFilter.DomainKeyword),
		DomainRegex:   slices.Clone(s.activeFilter.DomainRegex),
		ProcessName:   slices.Clone(s.activeFilter.ProcessName),
		PackageName:   slices.Clone(s.activeFilter.PackageName),
	}
}

// AddItem adds a new item to the filter of the given type.
func (s *SplitTunnel) AddItem(filterType, item string) error {
	if err := s.updateFilter(filterType, item, merge); err != nil {
		return err
	}
	s.log.Log(
		context.Background(), internal.LevelTrace, "added item to filter",
		"filterType", filterType, "item", item,
	)
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
	s.log.Log(
		context.Background(), internal.LevelTrace, "removed item from filter",
		"filterType", filterType, "item", item,
	)
	if err := s.saveToFile(); err != nil {
		return fmt.Errorf("writing rule to %s: %w", s.ruleFile, err)
	}
	return nil
}

// AddItems adds multiple items to the filter.
func (s *SplitTunnel) AddItems(items Filter) error {
	s.updateFilters(items, merge)
	s.log.Log(context.Background(), internal.LevelTrace, "added items to filter", "items", items.String())
	return s.saveToFile()
}

// RemoveItems removes multiple items from the filter.
func (s *SplitTunnel) RemoveItems(items Filter) error {
	s.updateFilters(items, remove)
	s.log.Log(context.Background(), internal.LevelTrace, "removed items from filter", "items", items.String())
	return s.saveToFile()
}

type Filter struct {
	Domain        []string
	DomainSuffix  []string
	DomainKeyword []string
	DomainRegex   []string
	ProcessName   []string
	PackageName   []string
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
	rs := O.PlainRuleSetCompat{
		Version: 3,
		Options: O.PlainRuleSet{
			Rules: []O.HeadlessRule{
				{
					Type:           "logical",
					LogicalOptions: s.rule,
				},
			},
		},
	}
	buf, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	return os.WriteFile(s.ruleFile, buf, 0644)
}

func (s *SplitTunnel) loadRule() error {
	content, err := os.ReadFile(s.ruleFile)
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
		s.log.Warn("split tunnel rule file format is invalid, using empty rule")
		return nil
	}

	s.rule = rules[0].LogicalOptions
	s.activeFilter = &(s.rule.Rules[1].DefaultOptions)

	s.log.Log(context.Background(), internal.LevelTrace, "loaded split tunnel rules",
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

var (
	initOnce sync.Once
)

// initSplitTunnel ensures that the split tunnel rule file exists to prevent sing-box from erroring
// because it can't find it.
func initSplitTunnel() {
	initOnce.Do(func() {
		ruleFile := filepath.Join(common.DataPath(), splitTunnelFile)
		if _, err := os.Stat(ruleFile); errors.Is(err, fs.ErrNotExist) {
			s := &SplitTunnel{
				rule:     defaultRule(),
				ruleFile: filepath.Join(common.DataPath(), splitTunnelTag+".json"),
				log:      slog.Default().With("service", "split-tunnel"),
			}
			s.log.Debug("Creating initial split tunnel rule file", "file", s.ruleFile)
			s.saveToFile()
		}
	})
}
