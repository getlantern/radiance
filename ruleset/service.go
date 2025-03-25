/*
Package ruleset provides functionality to add/remove filters to rulesets or enable/disable all rules
using the ruleset.

Currently, only supports [rule.LocalRuleSet] and the following filter types:
  - domain
  - domainSuffix
  - domainKeyword
  - domainRegex
  - processName
  - packageName

The [rule.LocalRuleSet] stores the filters in a file named <tag>.json at the provided dataPath.
*/
package ruleset

import "context"

// Manager allows creating and retrieving [MutableRuleSet]s.
type Manager struct {
	ctx      context.Context
	rulesets map[string]*MutableRuleSet
}

func NewService() *Manager {
	return &Manager{
		rulesets: make(map[string]*MutableRuleSet),
	}
}

// Start starts all the rulesets managed by the [Manager].
func (m *Manager) Start(ctx context.Context) error {
	m.ctx = ctx
	for _, r := range m.rulesets {
		if err := r.Start(ctx); err != nil {
			return err
		}
	}
	return nil
}

// NewMutableRuleSet creates a new [MutableRuleSet] to manage the [adapter.RuleSet] with the provided
// tag. The returned [MutableRuleSet] will store [adapter.RuleSet] filters in a file named <tag>.json
// in the provided dataPath. enable specifies whether the ruleset is initially enabled.
func (m *Manager) NewMutableRuleSet(dataPath, tag string, enable bool) *MutableRuleSet {
	m.rulesets[tag] = newMutableRuleSet(dataPath, tag, enable)
	return m.rulesets[tag]
}

// MutableRuleSet returns the [MutableRuleSet] with the provided tag.
func (m *Manager) MutableRuleSet(tag string) *MutableRuleSet {
	return m.rulesets[tag]
}
