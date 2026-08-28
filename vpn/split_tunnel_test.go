//go:build !novpn

package vpn

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/internal"
	rlog "github.com/getlantern/radiance/log"
)

func TestNewSplitTunnelHandlerQuarantinesInvalidRuleFile(t *testing.T) {
	tmpDir := t.TempDir()
	ruleFile := filepath.Join(tmpDir, internal.SplitTunnelFileName)
	// A rule file this build's sing-box can't decode — the shape a downgrade
	// leaves behind. Staged before construction so newSplitTunnel won't replace it.
	require.NoError(t, atomicfile.WriteFile(ruleFile,
		[]byte(`{"version":3,"rules":[{"unknown_field":true}]}`), 0o600))

	st, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.Error(t, err, "an unparseable rule file must be reported to the caller")
	require.NotNil(t, st, "handler must be usable despite the load error")

	// Falls back to the default no-op rule.
	assert.Empty(t, st.Filters().Domain)
	assert.False(t, st.IsEnabled())

	invalidPath := filepath.Join(tmpDir, internal.SplitTunnelInvalidFileName)
	assert.FileExists(t, invalidPath, "the unparseable file must be preserved for diagnostics")
	assert.FileExists(t, ruleFile, "split-tunnel.json must be left in place for re-upgrade")
}

func TestEnableDisableIsEnabled(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())

	if assert.NoError(t, st.SetEnabled(false)) {
		assert.False(t, st.IsEnabled(), "split tunnel should be disabled")
	}
	if assert.NoError(t, st.SetEnabled(true)) {
		assert.True(t, st.IsEnabled(), "split tunnel should be enabled")
	}
}

func TestAddRemoveItem(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())

	domain := "example.com"
	domain2 := "example2.com"
	packageName := "com.example"

	t.Run("adding domain item must update domain filter", func(t *testing.T) {
		require.NoError(t, st.AddItem(TypeDomain, domain))
		f := st.Filters()
		assert.Equal(t, []string{domain}, f.Domain)
	})

	t.Run("adding second domain must update the filter and contain both domains", func(t *testing.T) {
		require.NoError(t, st.AddItem(TypeDomain, domain2))
		f := st.Filters()
		assert.Equal(t, []string{domain, domain2}, f.Domain)
	})

	t.Run("adding package domain must update package filter", func(t *testing.T) {
		require.NoError(t, st.AddItem(TypePackageName, packageName))
		f := st.Filters()
		assert.Equal(t, []string{packageName}, f.PackageName)
	})

	t.Run("removing domain must update domain filter", func(t *testing.T) {
		require.NoError(t, st.RemoveItem(TypeDomain, domain))
		f := st.Filters()
		assert.NotContains(t, f.Domain, domain)
		assert.NotEmpty(t, f.PackageName)
	})
}

func TestRemoveItems(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())

	require.NoError(t, st.RemoveItems(SplitTunnelFilter{Domain: []string{"a.com"}, ProcessName: []string{"proc"}}))
	f := st.Filters()
	assert.Empty(t, f.Domain)
	assert.Empty(t, f.ProcessName)
}

func TestAddRemoveItems(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())

	items := SplitTunnelFilter{
		Domain:       []string{"a.com", "b.com"},
		DomainSuffix: []string{"suffix"},
		ProcessName:  []string{"proc"},
		PackageName:  []string{"pkg"},
	}
	err := st.AddItems(items)
	require.NoError(t, err)
	f := st.Filters()
	assert.ElementsMatch(t, []string{"a.com", "b.com"}, f.Domain)
	assert.Equal(t, []string{"suffix"}, f.DomainSuffix)
	assert.Equal(t, []string{"proc"}, f.ProcessName)
	assert.Equal(t, []string{"pkg"}, f.PackageName)

	err = st.RemoveItems(SplitTunnelFilter{Domain: []string{"a.com"}, ProcessName: []string{"proc"}})
	require.NoError(t, err)
	f = st.Filters()
	assert.Equal(t, []string{"b.com"}, f.Domain)
	assert.Empty(t, f.ProcessName)
}

// TestConcurrentMutationPersistsParseableRuleSet drives the mutators
// concurrently: they share activeFilter.Rules and rule.Mode, so without holding
// s.access across the mutate and the subsequent save, saveLocked reads those
// fields while another goroutine modifies them — flagged by -race and capable
// of serializing a torn rule set.
func TestConcurrentMutationPersistsParseableRuleSet(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for i := range 20 {
		wg.Go(func() {
			domain := fmt.Sprintf("d%d.example.com", i)
			record(st.AddItems(SplitTunnelFilter{Domain: []string{domain}, ProcessName: []string{fmt.Sprintf("p%d", i)}}))
			record(st.SetEnabled(i%2 == 0))
			record(st.RemoveItems(SplitTunnelFilter{Domain: []string{domain}}))
		})
	}
	wg.Wait()
	assert.Empty(t, errs, "concurrent mutations must all succeed")

	_, err = NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err, "rule set persisted under concurrent mutation must still parse")
}

func TestFilterPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, st.AddItem("domain", "example.com"))

	f := st.Filters()
	assert.Equal(t, []string{"example.com"}, f.Domain)

	st, err = NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	f = st.Filters()
	assert.Equal(t, []string{"example.com"}, f.Domain, "expected filters to persist after reloading from file")
}

func TestFilterPersistenceAfterLoad(t *testing.T) {
	tmpDir := t.TempDir()
	// Simulate the daemon path: NewSplitTunnelHandler (newSplitTunnel + loadRule), then AddItems
	st, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)

	require.NoError(t, st.AddItems(SplitTunnelFilter{Domain: []string{"example.com"}}))
	f := st.Filters()
	assert.Equal(t, []string{"example.com"}, f.Domain, "filter should be set in memory after AddItems")

	// Reload from disk to verify persistence
	st2, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	f = st2.Filters()
	assert.Equal(t, []string{"example.com"}, f.Domain, "filter should persist to disk after AddItems")
}

func TestUpdateFilterUnsupportedType(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	err := st.AddItem("unsupported", "foo")
	assert.Error(t, err)
}

func TestRemoveEdgeCases(t *testing.T) {
	// Remove from empty slice
	out := remove([]string{}, []string{"a"})
	assert.Empty(t, out)
	// Remove with empty items
	out = remove([]string{"a"}, []string{})
	assert.Equal(t, []string{"a"}, out)
	// Remove non-existent item
	out = remove([]string{"a"}, []string{"b"})
	assert.Equal(t, []string{"a"}, out)
	// Remove existing item
	out = remove([]string{"a", "b"}, []string{"a"})
	assert.Len(t, out, 1)
	assert.NotContains(t, out, "a")
	// Remove multiple items
	out = remove([]string{"a", "b", "c"}, []string{"a", "c"})
	assert.Equal(t, []string{"b"}, out)
}

func TestMatch(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, st.AddItem("domain", "example.com"))

	ruleOpts := O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{
				RuleSet: []string{splitTunnelTag},
			},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeRoute,
				RouteOptions: O.RouteActionOptions{
					Outbound: "direct",
				},
			},
		},
	}
	rsetOpts := O.RuleSet{
		Type: C.RuleSetTypeLocal,
		Tag:  splitTunnelTag,
		LocalOptions: O.LocalRuleSet{
			Path: st.ruleFile,
		},
		Format: C.RuleSetFormatSource,
	}

	ctx := service.ContextWithDefaultRegistry(context.Background())
	logger := log.NewNOPFactory().Logger()

	router := &mockRouter{}
	service.MustRegister[adapter.Router](ctx, router)
	service.MustRegister(ctx, new(adapter.NetworkManager))

	ruleSet, err := R.NewRuleSet(ctx, logger, rsetOpts)
	require.NoError(t, err)
	require.NoError(t, ruleSet.StartContext(ctx, new(adapter.HTTPStartContext)))
	defer ruleSet.Close()

	router.ruleSet = ruleSet

	rule, err := R.NewRule(ctx, logger, ruleOpts, false)
	require.NoError(t, err)
	require.NoError(t, rule.Start())
	defer rule.Close()

	metadata := &adapter.InboundContext{Domain: "example.com"}

	rsStr := ruleSet.String()
	require.NoError(t, st.SetEnabled(true))
	require.Eventually(t, func() bool {
		return ruleSet.String() != rsStr
	}, time.Second, 50*time.Millisecond, "timed out waiting for rule reload")

	assert.True(t, rule.Match(metadata), "rule should match when split tunnel is enabled")

	rsStr = ruleSet.String()
	require.NoError(t, st.SetEnabled(false))
	require.Eventually(t, func() bool {
		return ruleSet.String() != rsStr
	}, time.Second, 50*time.Millisecond, "timed out waiting for rule reload")

	assert.False(t, rule.Match(metadata), "rule should not match when split tunnel is not enabled")
}

type mockRouter struct {
	adapter.Router
	ruleSet adapter.RuleSet
}

func (r *mockRouter) RuleSet(tag string) (adapter.RuleSet, bool) {
	return r.ruleSet, true
}

// splitTunnelMatcher builds a live route rule from st's saved rule file.
// waitReload blocks until the rule set reflects the latest saved state.
// A match means the connection is routed direct.
func splitTunnelMatcher(t *testing.T, st *SplitTunnel) (rule adapter.Rule, waitReload func()) {
	t.Helper()
	ruleOpts := O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{RuleSet: []string{splitTunnelTag}},
			RuleAction: O.RuleAction{
				Action:       C.RuleActionTypeRoute,
				RouteOptions: O.RouteActionOptions{Outbound: "direct"},
			},
		},
	}
	rsetOpts := O.RuleSet{
		Type:         C.RuleSetTypeLocal,
		Tag:          splitTunnelTag,
		LocalOptions: O.LocalRuleSet{Path: st.ruleFile},
		Format:       C.RuleSetFormatSource,
	}

	ctx := service.ContextWithDefaultRegistry(context.Background())
	logger := log.NewNOPFactory().Logger()
	router := &mockRouter{}
	service.MustRegister[adapter.Router](ctx, router)
	service.MustRegister(ctx, new(adapter.NetworkManager))

	ruleSet, err := R.NewRuleSet(ctx, logger, rsetOpts)
	require.NoError(t, err)
	require.NoError(t, ruleSet.StartContext(ctx, new(adapter.HTTPStartContext)))
	t.Cleanup(func() { ruleSet.Close() })
	router.ruleSet = ruleSet

	rule, err = R.NewRule(ctx, logger, ruleOpts, false)
	require.NoError(t, err)
	require.NoError(t, rule.Start())
	t.Cleanup(func() { rule.Close() })

	last := ruleSet.String()
	waitReload = func() {
		t.Helper()
		require.Eventually(t, func() bool {
			return ruleSet.String() != last
		}, time.Second, 50*time.Millisecond, "timed out waiting for rule reload")
		last = ruleSet.String()
	}
	return rule, waitReload
}

func TestPolicyInvertsMatch(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, st.AddItem(TypeDomain, "example.com"))

	rule, waitReload := splitTunnelMatcher(t, st)
	matched := &adapter.InboundContext{Domain: "example.com"}
	unmatched := &adapter.InboundContext{Domain: "other.com"}

	require.NoError(t, st.SetEnabled(true))
	waitReload()

	assert.True(t, rule.Match(matched), "exclude: matched should route direct")
	assert.False(t, rule.Match(unmatched), "exclude: unmatched should stay tunneled")

	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))
	waitReload()

	assert.False(t, rule.Match(matched), "include: matched should stay tunneled")
	assert.True(t, rule.Match(unmatched), "include: unmatched should route direct")
}

func TestEmptyFilterModeInert(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, st.SetEnabled(true))
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))

	// Build the matcher after the state is set; an empty filter serializes to a
	// disable-only rule set whose stringified form doesn't change on the
	// enable/policy toggle, so there is no reload edge to wait on.
	rule, _ := splitTunnelMatcher(t, st)

	assert.False(t, rule.Match(&adapter.InboundContext{Domain: "anything.com"}),
		"empty include filter should tunnel everything (route nothing direct)")
}

func TestPolicyPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	require.Equal(t, SplitTunnelPolicyExclude, st.Policy(), "default policy should be exclude")

	require.NoError(t, st.AddItem(TypeDomain, "example.com"))
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))

	st2, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, SplitTunnelPolicyInclude, st2.Policy(), "policy should persist across reload")
	assert.ElementsMatch(t, []string{"example.com"}, st2.Filters().Domain)
}

func TestEmptyFilterPolicyReloadAndReconcile(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude)) // empty filter

	st2, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	require.Equal(t, SplitTunnelPolicyExclude, st2.Policy(), "empty-filter policy is not persisted across reload")

	// Reconcile from the durable intent, then add an item so it persists.
	require.NoError(t, st2.SetPolicy(SplitTunnelPolicyInclude))
	require.NoError(t, st2.AddItem(TypeDomain, "example.com"))

	st3, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, SplitTunnelPolicyInclude, st3.Policy(), "policy persists once the filter is non-empty")
}

func TestSetPolicyIdempotentAndDefaults(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.Equal(t, SplitTunnelPolicyExclude, st.Policy())

	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude)) // no-op when already in policy
	assert.Equal(t, SplitTunnelPolicyInclude, st.Policy())

	require.NoError(t, st.SetPolicy(SplitTunnelPolicyExclude))
	assert.Equal(t, SplitTunnelPolicyExclude, st.Policy())

	require.NoError(t, st.SetPolicy(SplitTunnelPolicy("bogus")), "unrecognized policy is treated as exclude")
	assert.Equal(t, SplitTunnelPolicyExclude, st.Policy())
}

func TestMigration(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())

	// Create a legacy format rule file
	legacyRule := O.LogicalHeadlessRule{
		Mode: C.LogicalTypeOr,
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
				Type: C.RuleTypeDefault,
				DefaultOptions: O.DefaultHeadlessRule{
					Domain:           []string{"example.com", "test.com"},
					DomainSuffix:     []string{".org"},
					DomainKeyword:    []string{"keyword"},
					DomainRegex:      []string{".*\\.io$"},
					PackageName:      []string{"com.example.app"},
					ProcessName:      []string{"chrome"},
					ProcessPath:      []string{"/usr/bin/firefox"},
					ProcessPathRegex: []string{"/opt/.*"},
				},
			},
		},
	}

	// Write legacy format to file
	rs := O.PlainRuleSetCompat{
		Version: 3,
		Options: O.PlainRuleSet{
			Rules: []O.HeadlessRule{
				{
					Type:           "logical",
					LogicalOptions: legacyRule,
				},
			},
		},
	}
	buf, err := json.Marshal(rs)
	require.NoError(t, err)
	err = atomicfile.WriteFile(st.ruleFile, buf, 0644)
	require.NoError(t, err)

	// Load the legacy format
	err = st.loadRule()
	require.NoError(t, err)
	want := `{
	"type": "logical",
	"mode": "or",
	"rules": [
		{
			"type": "logical",
			"mode": "and",
			"rules": [
				{
					"domain": "disable.rule"
				},
				{
					"domain": "disable.rule",
					"invert": true
				}
			]
		},
		{
			"type": "logical",
			"mode": "or",
			"rules": [
				{
					"domain": ["example.com", "test.com"],
					"domain_suffix": ".org",
					"domain_keyword": "keyword",
					"domain_regex": ".*\\.io$"
				},
				{
					"package_name": "com.example.app"
				},
				{
					"process_name": "chrome"
				},
				{
					"process_path": "/usr/bin/firefox"
				},
				{
					"process_path_regex": "/opt/.*"
				}
			]
		}
	]
}
`
	rule, _ := json.UnmarshalExtended[O.LogicalHeadlessRule]([]byte(want))
	assert.Equal(t, rule, st.rule)
}
