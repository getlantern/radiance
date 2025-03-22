package splittunnel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

const (
	TypeDomain        = "domain"
	TypeDomainSuffix  = "domainSuffix"
	TypeDomainKeyword = "domainKeyword"
	TypeDomainRegex   = "domainRegex"
	TypeProcessName   = "processName"
	TypePackageName   = "packageName"

	ruleFile = "split-tunnel.json"
)

type Manager struct {
	enabled  *atomic.Bool
	router   adapter.Router
	ruleset  adapter.RuleSet
	rules    []*splitTunnelRule
	filter   option.DefaultHeadlessRule
	filterMu sync.RWMutex

	ruleFile string
}

func NewManager(dataPath string, enable bool) *Manager {
	// TODO: accpet logger?
	path := filepath.Join(dataPath, ruleFile)
	enabled := new(atomic.Bool)
	enabled.Store(enable)
	return &Manager{
		enabled:  enabled,
		ruleFile: path,
	}
}

func (m *Manager) Enable() {
	m.enabled.Store(true)
}

func (m *Manager) Disable() {
	m.enabled.Store(false)
}

func (m *Manager) Start(ctx context.Context) error {
	router := service.FromContext[adapter.Router](ctx)
	ruleset, loaded := router.RuleSet("split-tunnel")
	if !loaded {
		return errors.New("split-tunnel RuleSet not found")
	}

	rules := router.Rules()
	for r, rule := range rules {
		if strings.Contains(rule.String(), "rule_set=split-tunnel") {
			s := &splitTunnelRule{
				Rule:    rule,
				enabled: m.enabled,
			}
			rules[r] = s
			m.rules = append(m.rules, s)
		}
	}

	m.loadRule(ruleset)
	ruleset.RegisterCallback(m.loadRule)
	return nil
}

func (m *Manager) AddItem(filterType string, item string) {
	m.filterMu.Lock()
	switch filterType {
	case TypeDomain:
		m.filter.Domain = append(m.filter.Domain, item)
	case TypeDomainSuffix:
		m.filter.DomainSuffix = append(m.filter.DomainSuffix, item)
	case TypeDomainKeyword:
		m.filter.DomainKeyword = append(m.filter.DomainKeyword, item)
	case TypeDomainRegex:
		m.filter.DomainRegex = append(m.filter.DomainRegex, item)
	case TypeProcessName:
		m.filter.ProcessName = append(m.filter.ProcessName, item)
	case TypePackageName:
		m.filter.PackageName = append(m.filter.PackageName, item)
	}
	m.filterMu.Unlock()
	m.saveToFile()
}

func (m *Manager) RemoveItem(filterType string, item string) {
	m.filterMu.Lock()
	switch filterType {
	case TypeDomain:
		m.filter.Domain = remove(m.filter.Domain, item)
	case TypeDomainSuffix:
		m.filter.DomainSuffix = remove(m.filter.DomainSuffix, item)
	case TypeDomainKeyword:
		m.filter.DomainKeyword = remove(m.filter.DomainKeyword, item)
	case TypeDomainRegex:
		m.filter.DomainRegex = remove(m.filter.DomainRegex, item)
	case TypeProcessName:
		m.filter.ProcessName = remove(m.filter.ProcessName, item)
	case TypePackageName:
		m.filter.PackageName = remove(m.filter.PackageName, item)
	}
	m.filterMu.Unlock()
	m.saveToFile()
}

func remove(slice []string, item string) []string {
	for i, v := range slice {
		if v == item {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func (m *Manager) saveToFile() error {
	m.filterMu.RLock()
	buf, err := json.Marshal(m.filter)
	m.filterMu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(m.ruleFile, buf, 0644)
}

func (m *Manager) loadRule(s adapter.RuleSet) {
	filters := s.String()
	rule, _ := json.UnmarshalExtended[option.HeadlessRule]([]byte(filters))
	m.filterMu.Lock()
	m.filter = rule.DefaultOptions
	m.filterMu.Unlock()
}

func (m *Manager) Filters() string {
	m.filterMu.RLock()
	defer m.filterMu.RUnlock()
	buf, _ := json.Marshal(m.filter)
	return string(buf)
}
