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
	"reflect"
	"slices"
	"sync"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
	"github.com/getlantern/radiance/internal"
)

// This impossible regexp keeps empty filters readable by older rule-set loaders.
const emptySplitTunnelPattern = "a^"

// SplitTunnel owns the persisted split-tunnel policy and selections.
type SplitTunnel struct {
	access       sync.Mutex
	state        SplitTunnelState
	ruleFile     string
	legacyPolicy bool
	legacyFormat bool
	logger       *slog.Logger
}

// NewSplitTunnelHandler loads the saved policy and selections.
// On error, the returned manager remains usable with split tunneling disabled.
func NewSplitTunnelHandler(dataPath string, logger *slog.Logger) (*SplitTunnel, error) {
	s := newSplitTunnel(dataPath, logger)
	if err := s.loadRule(); err != nil {
		return s, fmt.Errorf("loading split tunnel rule file %s: %w", s.ruleFile, err)
	}
	return s, nil
}

func newSplitTunnel(path string, logger *slog.Logger) *SplitTunnel {
	s := &SplitTunnel{
		state:    SplitTunnelState{Policy: SplitTunnelPolicyExclude},
		ruleFile: filepath.Join(path, internal.SplitTunnelFileName),
		logger:   logger,
	}
	if _, err := os.Stat(s.ruleFile); errors.Is(err, fs.ErrNotExist) {
		if err := s.save(s.state); err != nil {
			logger.Error("Writing initial split tunnel rule file", "file", s.ruleFile, "error", err)
		}
	}
	return s
}

// State returns an independent snapshot of the persisted policy and selections.
func (s *SplitTunnel) State() SplitTunnelState {
	s.access.Lock()
	defer s.access.Unlock()
	state := s.state
	state.Filters = state.Filters.clone()
	return state
}

// IsEnabled reports whether split tunneling is enabled.
func (s *SplitTunnel) IsEnabled() bool {
	s.access.Lock()
	defer s.access.Unlock()
	return s.state.Enabled
}

// SwitchPolicy changes policy and clears all selections; the current policy is a no-op.
func (s *SplitTunnel) SwitchPolicy(policy SplitTunnelPolicy) error {
	return s.mutate(func(state *SplitTunnelState) error {
		if state.Policy != policy {
			state.Policy = policy
			state.Filters = SplitTunnelFilter{}
		}
		return nil
	})
}

// Configure persists the non-nil enabled and policy values together without changing selections.
func (s *SplitTunnel) Configure(enabled *bool, policy *SplitTunnelPolicy) error {
	return s.mutate(func(state *SplitTunnelState) error {
		if enabled != nil {
			state.Enabled = *enabled
		}
		if policy != nil {
			state.Policy = *policy
		}
		return nil
	})
}

// ImportLegacyPolicy rewrites a rule file from an older build, using policy only when the file did not record one.
func (s *SplitTunnel) ImportLegacyPolicy(policy SplitTunnelPolicy) error {
	s.access.Lock()
	defer s.access.Unlock()
	if !s.legacyPolicy && !s.legacyFormat {
		return nil
	}
	next := s.state
	if s.legacyPolicy {
		if !policy.Valid() {
			policy = SplitTunnelPolicyExclude
		}
		next.Policy = policy
	}
	return s.commitLocked(next)
}

// ClearApps removes every app selector while preserving website selections.
func (s *SplitTunnel) ClearApps() error {
	return s.mutate(func(state *SplitTunnelState) error {
		state.Filters.ProcessName = nil
		state.Filters.ProcessPath = nil
		state.Filters.ProcessPathRegex = nil
		state.Filters.PackageName = nil
		return nil
	})
}

// AddItems adds selections without duplicating existing entries.
func (s *SplitTunnel) AddItems(items SplitTunnelFilter) error {
	return s.mutate(func(state *SplitTunnelState) error {
		state.Filters.update(items, merge)
		return nil
	})
}

// RemoveItems removes the supplied selections.
func (s *SplitTunnel) RemoveItems(items SplitTunnelFilter) error {
	return s.mutate(func(state *SplitTunnelState) error {
		state.Filters.update(items, remove)
		return nil
	})
}

func (s *SplitTunnel) mutate(fn func(*SplitTunnelState) error) error {
	s.access.Lock()
	defer s.access.Unlock()
	next := s.state
	next.Filters = next.Filters.clone()
	if err := fn(&next); err != nil {
		return err
	}
	next.Filters.DomainRegex = remove(next.Filters.DomainRegex, []string{emptySplitTunnelPattern})
	if reflect.DeepEqual(next, s.state) {
		return nil
	}
	return s.commitLocked(next)
}

func (s *SplitTunnel) commitLocked(next SplitTunnelState) error {
	if err := s.save(next); err != nil {
		return err
	}
	s.state = next
	s.legacyPolicy = false
	s.legacyFormat = false
	return nil
}

func merge(selected []string, items []string) []string {
	for _, item := range items {
		if !slices.Contains(selected, item) {
			selected = append(selected, item)
		}
	}
	return selected
}

func remove(selected []string, items []string) []string {
	selected = slices.DeleteFunc(selected, func(item string) bool { return slices.Contains(items, item) })
	if len(selected) == 0 {
		return nil
	}
	return selected
}

func (s *SplitTunnel) save(state SplitTunnelState) error {
	if !state.Policy.Valid() {
		return fmt.Errorf("invalid split-tunnel policy: %q", state.Policy)
	}
	rule, err := compileSplitTunnelRule(state)
	if err != nil {
		return err
	}
	buf, err := json.Marshal(O.PlainRuleSetCompat{
		Version: 3,
		Options: O.PlainRuleSet{Rules: []O.HeadlessRule{rule}},
	})
	if err != nil {
		return fmt.Errorf("marshalling rule set: %w", err)
	}
	if err := atomicfile.WriteFile(s.ruleFile, buf, fileperm.File); err != nil {
		return fmt.Errorf("writing rule file %s: %w", s.ruleFile, err)
	}
	return nil
}

func compileSplitTunnelRule(state SplitTunnelState) (O.HeadlessRule, error) {
	rule := O.HeadlessRule{Type: C.RuleTypeLogical, LogicalOptions: splitTunnelRule(state)}
	if _, err := R.NewHeadlessRule(context.Background(), rule); err != nil {
		return rule, fmt.Errorf("compiling split tunnel rule: %w", err)
	}
	return rule, nil
}

func splitTunnelRule(state SplitTunnelState) O.LogicalHeadlessRule {
	root := defaultRule()
	if state.Enabled {
		root.Mode = C.LogicalTypeOr
	}
	filter := &root.Rules[1].LogicalOptions
	filter.Invert = state.Policy == SplitTunnelPolicyInclude
	for _, rule := range []O.DefaultHeadlessRule{
		{Domain: state.Filters.Domain, DomainSuffix: state.Filters.DomainSuffix,
			DomainKeyword: state.Filters.DomainKeyword, DomainRegex: state.Filters.DomainRegex},
		{PackageName: state.Filters.PackageName},
		{ProcessName: state.Filters.ProcessName},
		{ProcessPath: state.Filters.ProcessPath},
		{ProcessPathRegex: state.Filters.ProcessPathRegex},
	} {
		if !isEmptyRule(rule) {
			filter.Rules = append(filter.Rules, O.HeadlessRule{Type: C.RuleTypeDefault, DefaultOptions: rule})
		}
	}
	if len(filter.Rules) == 0 {
		filter.Rules = []O.HeadlessRule{{
			Type:           C.RuleTypeDefault,
			DefaultOptions: O.DefaultHeadlessRule{DomainRegex: []string{emptySplitTunnelPattern}},
		}}
	}
	return root
}

func isEmptyRule(rule O.DefaultHeadlessRule) bool {
	return len(rule.Domain) == 0 && len(rule.DomainSuffix) == 0 &&
		len(rule.DomainKeyword) == 0 && len(rule.DomainRegex) == 0 &&
		len(rule.ProcessName) == 0 && len(rule.PackageName) == 0 &&
		len(rule.ProcessPath) == 0 && len(rule.ProcessPathRegex) == 0
}

func parseSplitTunnelRule(rules []O.HeadlessRule) (state SplitTunnelState, err error) {
	if len(rules) != 1 || rules[0].Type != C.RuleTypeLogical {
		return state, errors.New("expected a single logical split tunnel rule")
	}
	root := rules[0].LogicalOptions
	if root.Invert || (root.Mode != C.LogicalTypeAnd && root.Mode != C.LogicalTypeOr) {
		return state, errors.New("invalid split tunnel rule mode")
	}
	if len(root.Rules) < 1 || len(root.Rules) > 2 ||
		!reflect.DeepEqual(root.Rules[0], defaultRule().Rules[0]) {
		return state, errors.New("invalid split tunnel disable rule")
	}
	state = SplitTunnelState{Enabled: root.Mode == C.LogicalTypeOr, Policy: SplitTunnelPolicyExclude}
	if len(root.Rules) == 2 {
		filter := root.Rules[1]
		children := []O.HeadlessRule{filter}
		if filter.Type == C.RuleTypeLogical {
			if filter.LogicalOptions.Mode != C.LogicalTypeOr {
				return state, errors.New("invalid split tunnel filter mode")
			}
			children = filter.LogicalOptions.Rules
			if filter.LogicalOptions.Invert {
				state.Policy = SplitTunnelPolicyInclude
			}
		}
		for _, child := range children {
			if child.Type != C.RuleTypeDefault {
				return state, errors.New("invalid split tunnel filter rule")
			}
			rule := child.DefaultOptions
			if !supportedSplitTunnelRule(rule) {
				return state, errors.New("unsupported split tunnel filter conditions")
			}
			state.Filters.update(SplitTunnelFilter{
				Domain: rule.Domain, DomainSuffix: rule.DomainSuffix,
				DomainKeyword: rule.DomainKeyword, DomainRegex: rule.DomainRegex,
				PackageName: rule.PackageName, ProcessName: rule.ProcessName,
				ProcessPath: rule.ProcessPath, ProcessPathRegex: rule.ProcessPathRegex,
			}, merge)
		}
	}
	state.Filters.DomainRegex = remove(state.Filters.DomainRegex, []string{emptySplitTunnelPattern})
	return state, nil
}

func supportedSplitTunnelRule(rule O.DefaultHeadlessRule) bool {
	return reflect.DeepEqual(rule, O.DefaultHeadlessRule{
		Domain: rule.Domain, DomainSuffix: rule.DomainSuffix,
		DomainKeyword: rule.DomainKeyword, DomainRegex: rule.DomainRegex,
		PackageName: rule.PackageName, ProcessName: rule.ProcessName,
		ProcessPath: rule.ProcessPath, ProcessPathRegex: rule.ProcessPathRegex,
	})
}

func (s *SplitTunnel) loadRule() error {
	raw, err := atomicfile.ReadFile(s.ruleFile)
	if err != nil {
		return fmt.Errorf("reading rule file %s: %w", s.ruleFile, err)
	}
	rs, err := json.UnmarshalExtended[O.PlainRuleSetCompat](raw)
	if err != nil {
		s.quarantineInvalidRuleSet(raw)
		return fmt.Errorf("unmarshalling rule file %s: %w", s.ruleFile, err)
	}
	next, err := parseSplitTunnelRule(rs.Options.Rules)
	if err != nil {
		s.quarantineInvalidRuleSet(raw)
		return fmt.Errorf("validating rule file %s: %w", s.ruleFile, err)
	}
	if _, err := compileSplitTunnelRule(next); err != nil {
		s.quarantineInvalidRuleSet(raw)
		return err
	}
	s.access.Lock()
	root := rs.Options.Rules[0].LogicalOptions
	s.state = next
	s.legacyPolicy = len(root.Rules) == 1
	s.legacyFormat = len(root.Rules) == 2 && root.Rules[1].Type == C.RuleTypeDefault
	s.access.Unlock()
	return nil
}

func (s *SplitTunnel) quarantineInvalidRuleSet(content []byte) {
	invalidPath := filepath.Join(filepath.Dir(s.ruleFile), internal.SplitTunnelInvalidFileName)
	if err := atomicfile.WriteFile(invalidPath, content, fileperm.File); err != nil {
		s.logger.Error("Writing invalid split tunnel copy", "path", invalidPath, "error", err)
		return
	}
	s.logger.Warn("Saved invalid split tunnel rules for diagnostics", "path", invalidPath)
}

func defaultRule() O.LogicalHeadlessRule {
	// The first child is always false; AND disables routing and OR enables it.
	return O.LogicalHeadlessRule{
		Mode: C.LogicalTypeAnd,
		Rules: []O.HeadlessRule{
			{
				Type: C.RuleTypeLogical,
				LogicalOptions: O.LogicalHeadlessRule{
					Mode: C.LogicalTypeAnd,
					Rules: []O.HeadlessRule{
						{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultHeadlessRule{Domain: []string{"disable.rule"}}},
						{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultHeadlessRule{Domain: []string{"disable.rule"}, Invert: true}},
					},
				},
			},
			{Type: C.RuleTypeLogical, LogicalOptions: O.LogicalHeadlessRule{Mode: C.LogicalTypeOr}},
		},
	}
}
