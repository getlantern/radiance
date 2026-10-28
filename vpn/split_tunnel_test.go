//go:build !novpn

package vpn

import (
	"context"
	"fmt"
	"os"
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
	assert.FileExists(t, ruleFile, "loading must not remove the source file")
}

func TestInvalidCopySurvivesSave(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		wantErr string
	}{
		{"unparseable", `{"version":3,"rules":[{"unknown_field":true}]}`, "unmarshalling rule file"},
		{"invalid envelope", `{"version":3,"rules":[{"domain":"example.com"}]}`, "validating rule file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, internal.SplitTunnelFileName)
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
			require.ErrorContains(t, err, tt.wantErr)
			require.NotNil(t, st)

			require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))
			assert.Equal(t, SplitTunnelPolicyInclude, st.Policy())
			assert.False(t, st.IsEnabled())
			assert.Empty(t, st.Filters().Domain)
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.NotEqual(t, tt.content, string(current))
			_, err = json.UnmarshalExtended[O.PlainRuleSetCompat](current)
			require.NoError(t, err)
			saved, err := os.ReadFile(filepath.Join(dir, internal.SplitTunnelInvalidFileName))
			require.NoError(t, err)
			assert.Equal(t, tt.content, string(saved))
		})
	}
}

func writeRuleSet(t *testing.T, dir string, rs O.PlainRuleSet) []byte {
	t.Helper()
	content, err := json.Marshal(O.PlainRuleSetCompat{Version: 3, Options: rs})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, internal.SplitTunnelFileName), content, 0o600))
	return content
}

func TestRejectInvalidEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*O.PlainRuleSet)
	}{
		{"no root", func(rs *O.PlainRuleSet) { rs.Rules = nil }},
		{"plain root", func(rs *O.PlainRuleSet) {
			rs.Rules[0] = O.HeadlessRule{
				Type: C.RuleTypeDefault, DefaultOptions: O.DefaultHeadlessRule{Domain: []string{"example.com"}},
			}
		}},
		{"multiple roots", func(rs *O.PlainRuleSet) { rs.Rules = append(rs.Rules, rs.Rules[0]) }},
		{"empty logical root", func(rs *O.PlainRuleSet) { rs.Rules[0].LogicalOptions.Rules = nil }},
		{"inverted root", func(rs *O.PlainRuleSet) { rs.Rules[0].LogicalOptions.Invert = true }},
		{"invalid root mode", func(rs *O.PlainRuleSet) { rs.Rules[0].LogicalOptions.Mode = "xor" }},
		{"missing disable rule", func(rs *O.PlainRuleSet) {
			rs.Rules[0].LogicalOptions.Rules = rs.Rules[0].LogicalOptions.Rules[1:]
		}},
		{"invalid disable rule", func(rs *O.PlainRuleSet) {
			rs.Rules[0].LogicalOptions.Rules[0].LogicalOptions.Mode = C.LogicalTypeOr
		}},
		{"extra child", func(rs *O.PlainRuleSet) {
			root := &rs.Rules[0].LogicalOptions
			root.Rules = append(root.Rules, root.Rules[1])
		}},
		{"invalid filter mode", func(rs *O.PlainRuleSet) {
			rs.Rules[0].LogicalOptions.Rules[1].LogicalOptions.Mode = C.LogicalTypeAnd
		}},
		{"unsupported filter condition", func(rs *O.PlainRuleSet) {
			rs.Rules[0].LogicalOptions.Rules[1].LogicalOptions.Rules = []O.HeadlessRule{
				{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultHeadlessRule{IPCIDR: []string{"10.0.0.0/8"}}},
			}
		}},
		{"inverted filter leaf", func(rs *O.PlainRuleSet) {
			rs.Rules[0].LogicalOptions.Rules[1].LogicalOptions.Rules = []O.HeadlessRule{
				{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultHeadlessRule{Domain: []string{"example.com"}, Invert: true}},
			}
		}},
		{"nested filter", func(rs *O.PlainRuleSet) {
			root := &rs.Rules[0].LogicalOptions
			root.Rules[1].LogicalOptions.Rules = []O.HeadlessRule{root.Rules[0]}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rs := O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeLogical, LogicalOptions: defaultRule()}}}
			tt.edit(&rs)
			dir := t.TempDir()
			content := writeRuleSet(t, dir, rs)
			_, err := json.UnmarshalExtended[O.PlainRuleSetCompat](content)
			require.NoError(t, err, "fixture must decode before envelope validation")

			st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
			require.ErrorContains(t, err, "validating rule file")
			require.NotNil(t, st)
			assert.False(t, st.IsEnabled())
			assert.Equal(t, SplitTunnelPolicyExclude, st.Policy())
			assert.Empty(t, st.Filters().Domain)
			for _, name := range []string{internal.SplitTunnelFileName, internal.SplitTunnelInvalidFileName} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)
				assert.Equal(t, content, got)
			}
			require.NoError(t, st.AddItem(TypeDomain, "example.org"))
			assert.Equal(t, []string{"example.org"}, st.Filters().Domain)
		})
	}
}

func TestLoadEquivalentDisableRule(t *testing.T) {
	content := `{"version":3,"rules":[{"type":"logical","mode":"or","rules":[
		{"type":"logical","mode":"and","invert":false,"rules":[
			{"domain":"disable.rule","domain_suffix":[]},
			{"domain":["disable.rule"],"domain_suffix":[],"invert":true}
		]},
		{"type":"logical","mode":"or","rules":[{"domain":"example.com"}]}
	]}]}`
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, internal.SplitTunnelFileName), []byte(content), 0o600))

	st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.True(t, st.IsEnabled())
	assert.Equal(t, []string{"example.com"}, st.Filters().Domain)
	assert.NoFileExists(t, filepath.Join(dir, internal.SplitTunnelInvalidFileName))
}

func TestLoadEmptyEnvelope(t *testing.T) {
	for _, children := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d children", children), func(t *testing.T) {
			root := defaultRule()
			root.Mode = C.LogicalTypeOr
			root.Rules = root.Rules[:children]
			dir := t.TempDir()
			writeRuleSet(t, dir, O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeLogical, LogicalOptions: root}}})

			st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
			require.NoError(t, err)
			assert.True(t, st.IsEnabled())
			assert.Empty(t, st.Filters().Domain)
			require.NoError(t, st.AddItem(TypeDomain, "example.org"))
			reloaded, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
			require.NoError(t, err)
			assert.Equal(t, []string{"example.org"}, reloaded.Filters().Domain)
		})
	}
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
			policy := SplitTunnelPolicyExclude
			if i%2 == 0 {
				policy = SplitTunnelPolicyInclude
			}
			record(st.SwitchPolicy(policy))
			record(st.RemoveItems(SplitTunnelFilter{Domain: []string{domain}}))
		})
	}
	wg.Wait()
	assert.Empty(t, errs, "concurrent mutations must all succeed")

	reloaded, err := NewSplitTunnelHandler(tmpDir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, st.State(), reloaded.State())
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

func TestEmptyIncludeRoutesDirect(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, st.SetEnabled(true))
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))

	rule, _ := splitTunnelMatcher(t, st)

	assert.True(t, rule.Match(&adapter.InboundContext{Domain: "anything.com"}))
	assert.True(t, rule.Match(&adapter.InboundContext{Domain: "disable.rule"}))
	assert.True(t, rule.Match(&adapter.InboundContext{}))
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

func TestEmptyPolicyPersistence(t *testing.T) {
	for _, policy := range []SplitTunnelPolicy{SplitTunnelPolicyExclude, SplitTunnelPolicyInclude} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", policy, enabled), func(t *testing.T) {
				dir := t.TempDir()
				st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
				require.NoError(t, err)
				require.NoError(t, st.Configure(&enabled, &policy))
				reloaded, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
				require.NoError(t, err)
				assert.Equal(t, st.State(), reloaded.State())
				rule, _ := splitTunnelMatcher(t, reloaded)
				assert.Equal(t, enabled && policy == SplitTunnelPolicyInclude,
					rule.Match(&adapter.InboundContext{Domain: "anything.com"}))
			})
		}
	}
}

func TestSetPolicyIdempotentAndValidation(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.Equal(t, SplitTunnelPolicyExclude, st.Policy())

	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude)) // no-op when already in policy
	assert.Equal(t, SplitTunnelPolicyInclude, st.Policy())

	require.NoError(t, st.SetPolicy(SplitTunnelPolicyExclude))
	assert.Equal(t, SplitTunnelPolicyExclude, st.Policy())

	require.Error(t, st.SetPolicy(SplitTunnelPolicy("bogus")))
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
	assert.Equal(t, rule, splitTunnelRule(st.State()))
}

func allSplitTunnelFilters() SplitTunnelFilter {
	return SplitTunnelFilter{
		Domain: []string{"example.com"}, DomainSuffix: []string{".ir"},
		DomainKeyword: []string{"keyword"}, DomainRegex: []string{"^example"},
		ProcessName: []string{"uninstalled"}, ProcessPath: []string{"/missing/app"},
		ProcessPathRegex: []string{"/Applications/Browser.*"}, PackageName: []string{"org.example.app"},
	}
}

func TestSwitchPolicyClearsOnce(t *testing.T) {
	dir := t.TempDir()
	st := newSplitTunnel(dir, rlog.NoOpLogger())
	require.NoError(t, st.AddItems(allSplitTunnelFilters()))
	require.NoError(t, st.SetEnabled(true))
	require.NoError(t, st.SwitchPolicy(SplitTunnelPolicyInclude))
	assert.Equal(t, SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude}, st.State())

	require.NoError(t, st.AddItems(allSplitTunnelFilters()))
	want := st.State()
	require.NoError(t, st.SwitchPolicy(SplitTunnelPolicyInclude))
	assert.Equal(t, want, st.State())
	require.Error(t, st.SwitchPolicy("invalid"))
	assert.Equal(t, want, st.State())
	reloaded, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, want, reloaded.State())

	require.NoError(t, st.SwitchPolicy(SplitTunnelPolicyExclude))
	assert.Equal(t, SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyExclude}, st.State())
}

func TestClearAppsPreservesWebsites(t *testing.T) {
	dir := t.TempDir()
	st := newSplitTunnel(dir, rlog.NoOpLogger())
	items := allSplitTunnelFilters()
	require.NoError(t, st.AddItems(items))
	require.NoError(t, st.SetPolicy(SplitTunnelPolicyInclude))
	require.NoError(t, st.ClearApps())
	items.ProcessName, items.ProcessPath, items.ProcessPathRegex, items.PackageName = nil, nil, nil, nil
	assert.Equal(t, items, st.Filters())
	assert.Equal(t, SplitTunnelPolicyInclude, st.Policy())
	reloaded, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, st.State(), reloaded.State())
}

func TestSplitTunnelMutationFailuresPreserveState(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, st.AddItems(allSplitTunnelFilters()))
	want := st.State()
	path := st.ruleFile
	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	st.ruleFile = t.TempDir()

	for name, mutate := range map[string]func() error{
		"enable": func() error { return st.SetEnabled(true) },
		"policy": func() error { return st.SetPolicy(SplitTunnelPolicyInclude) },
		"switch": func() error { return st.SwitchPolicy(SplitTunnelPolicyInclude) },
		"clear":  st.ClearApps,
		"add":    func() error { return st.AddItem(TypeDomain, "new.example") },
		"remove": func() error { return st.RemoveItems(allSplitTunnelFilters()) },
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, mutate())
			assert.Equal(t, want, st.State())
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, persisted, got)
		})
	}
}

func TestSplitTunnelRejectsInvalidMatchers(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	require.NoError(t, st.AddItems(allSplitTunnelFilters()))
	want := st.State()
	for _, items := range []SplitTunnelFilter{
		{DomainSuffix: []string{""}},
		{DomainRegex: []string{"("}},
		{ProcessPathRegex: []string{"("}},
	} {
		require.Error(t, st.AddItems(items))
		assert.Equal(t, want, st.State())
	}
}

func TestSplitTunnelSnapshotIsIndependent(t *testing.T) {
	st := newSplitTunnel(t.TempDir(), rlog.NoOpLogger())
	items := allSplitTunnelFilters()
	require.NoError(t, st.AddItems(items))
	items.Domain[0] = "changed.example"
	snapshot := st.State()
	snapshot.Filters.PackageName[0] = "changed.package"
	assert.Equal(t, allSplitTunnelFilters(), st.Filters())
}

func TestImportLegacyPolicyOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	root := defaultRule()
	root.Mode = C.LogicalTypeOr
	root.Rules = root.Rules[:1]
	writeRuleSet(t, dir, O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeLogical, LogicalOptions: root}}})
	st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, st.ImportLegacyPolicy(SplitTunnelPolicyInclude))
	assert.True(t, st.IsEnabled())
	assert.Equal(t, SplitTunnelPolicyInclude, st.Policy())
	require.NoError(t, st.AddItem(TypeDomain, "example.com"))
	require.NoError(t, st.ImportLegacyPolicy(SplitTunnelPolicyExclude))
	assert.Equal(t, SplitTunnelPolicyInclude, st.Policy())
	assert.Equal(t, []string{"example.com"}, st.Filters().Domain)

	require.NoError(t, st.SwitchPolicy(SplitTunnelPolicyExclude))
	st, err = NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, st.ImportLegacyPolicy(SplitTunnelPolicyInclude))
	assert.Equal(t, SplitTunnelPolicyExclude, st.Policy())
	assert.Empty(t, st.Filters().Domain)
}

func TestSplitTunnelFileCompatibility(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, policy := range []SplitTunnelPolicy{SplitTunnelPolicyExclude, SplitTunnelPolicyInclude} {
			state := SplitTunnelState{Enabled: true, Policy: policy}
			if !empty {
				state.Filters = allSplitTunnelFilters()
			}
			root := splitTunnelRule(state)
			require.NoError(t, validateSplitTunnelRule([]O.HeadlessRule{{Type: C.RuleTypeLogical, LogicalOptions: root}}))
			assert.False(t, root.Invert)
			require.Len(t, root.Rules, 2)
			assert.Equal(t, defaultRule().Rules[0], root.Rules[0])
			filter := root.Rules[1].LogicalOptions
			assert.Equal(t, C.LogicalTypeOr, filter.Mode)
			assert.Equal(t, policy == SplitTunnelPolicyInclude, filter.Invert)
			for _, child := range filter.Rules {
				assert.Equal(t, C.RuleTypeDefault, child.Type, "older readers only allow default filter children")
			}
			if empty {
				matcher, err := R.NewHeadlessRule(context.Background(), O.HeadlessRule{Type: C.RuleTypeLogical, LogicalOptions: root})
				require.NoError(t, err)
				for _, domain := range []string{"", "a", "a^", "example.com", "disable.rule"} {
					assert.Equal(t, policy == SplitTunnelPolicyInclude, matcher.Match(&adapter.InboundContext{Domain: domain}))
				}
			}
		}
	}
}

func TestLoadEmptyMatcherAfterLegacyMutation(t *testing.T) {
	dir := t.TempDir()
	root := splitTunnelRule(SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude})
	root.Rules[1].LogicalOptions.Rules[0].DefaultOptions.Domain = []string{"example.com"}
	writeRuleSet(t, dir, O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeLogical, LogicalOptions: root}}})
	st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, SplitTunnelFilter{Domain: []string{"example.com"}}, st.Filters())
	assert.Equal(t, SplitTunnelPolicyInclude, st.Policy())
}

func TestSplitTunnelAppOrWebsite(t *testing.T) {
	for _, policy := range []SplitTunnelPolicy{SplitTunnelPolicyExclude, SplitTunnelPolicyInclude} {
		for _, enabled := range []bool{false, true} {
			state := SplitTunnelState{
				Enabled: enabled, Policy: policy,
				Filters: SplitTunnelFilter{DomainSuffix: []string{".ir"}, PackageName: []string{"org.example.app"}},
			}
			rule, err := R.NewHeadlessRule(context.Background(), O.HeadlessRule{
				Type: C.RuleTypeLogical, LogicalOptions: splitTunnelRule(state),
			})
			require.NoError(t, err)
			for _, tc := range []struct {
				name     string
				metadata adapter.InboundContext
				selected bool
			}{
				{"website", adapter.InboundContext{Domain: "bank.co.ir"}, true},
				{"app", adapter.InboundContext{Domain: "other.com", ProcessInfo: &adapter.ConnectionOwner{AndroidPackageNames: []string{"org.example.app"}}}, true},
				{"both", adapter.InboundContext{Domain: "bank.co.ir", ProcessInfo: &adapter.ConnectionOwner{AndroidPackageNames: []string{"org.example.app"}}}, true},
				{"neither", adapter.InboundContext{Domain: "other.com", ProcessInfo: &adapter.ConnectionOwner{AndroidPackageNames: []string{"other.app"}}}, false},
				{"suffix boundary", adapter.InboundContext{Domain: "example.ir.com"}, false},
				{"unknown", adapter.InboundContext{}, false},
			} {
				t.Run(fmt.Sprintf("%s/%v/%s", policy, enabled, tc.name), func(t *testing.T) {
					wantDirect := tc.selected
					if policy == SplitTunnelPolicyInclude {
						wantDirect = !wantDirect
					}
					assert.Equal(t, enabled && wantDirect, rule.Match(&tc.metadata))
				})
			}
		}
	}
}

func TestRemoveLastSplitTunnelItemsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := newSplitTunnel(dir, rlog.NoOpLogger())
	require.NoError(t, st.AddItems(allSplitTunnelFilters()))
	require.NoError(t, st.RemoveItems(allSplitTunnelFilters()))
	assert.Equal(t, SplitTunnelFilter{}, st.Filters())
	reloaded, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, st.State(), reloaded.State())
}

func TestSplitTunnelEmptyMarkerIsNotASelection(t *testing.T) {
	dir := t.TempDir()
	st := newSplitTunnel(dir, rlog.NoOpLogger())
	require.NoError(t, st.AddItem(TypeDomainRegex, emptySplitTunnelPattern))
	assert.Equal(t, SplitTunnelFilter{}, st.Filters())
	require.NoError(t, st.AddItems(SplitTunnelFilter{DomainRegex: []string{emptySplitTunnelPattern, "^example"}}))
	assert.Equal(t, []string{"^example"}, st.Filters().DomainRegex)
	reloaded, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, st.State(), reloaded.State())
}

func TestImportLegacyPolicyPersistsCombinedFilterMigration(t *testing.T) {
	dir := t.TempDir()
	root := defaultRule()
	root.Mode = C.LogicalTypeOr
	root.Rules[1] = O.HeadlessRule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultHeadlessRule{
			Domain: []string{"example.com"}, PackageName: []string{"org.example.app"},
		},
	}
	writeRuleSet(t, dir, O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeLogical, LogicalOptions: root}}})
	st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	before := st.State()
	require.NoError(t, st.ImportLegacyPolicy(SplitTunnelPolicyInclude))
	assert.Equal(t, before, st.State())

	rule, _ := splitTunnelMatcher(t, st)
	assert.True(t, rule.Match(&adapter.InboundContext{Domain: "example.com"}))
	assert.True(t, rule.Match(&adapter.InboundContext{
		Domain: "other.com", ProcessInfo: &adapter.ConnectionOwner{AndroidPackageNames: []string{"org.example.app"}},
	}))
	assert.False(t, rule.Match(&adapter.InboundContext{Domain: "other.com"}))

	reloaded, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, before, reloaded.State())
	assert.False(t, reloaded.legacyFormat)
	assert.False(t, reloaded.legacyPolicy)
	reloaded.ruleFile = t.TempDir()
	require.NoError(t, reloaded.ImportLegacyPolicy(SplitTunnelPolicyInclude), "canonical files must not be rewritten")
	assert.Equal(t, before, reloaded.State())
}

func TestImportLegacyFilterMigrationRetriesFailedWrite(t *testing.T) {
	dir := t.TempDir()
	root := defaultRule()
	root.Rules[1] = O.HeadlessRule{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultHeadlessRule{Domain: []string{"example.com"}}}
	writeRuleSet(t, dir, O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeLogical, LogicalOptions: root}}})
	st, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	before := st.State()
	path := st.ruleFile
	st.ruleFile = t.TempDir()
	require.Error(t, st.ImportLegacyPolicy(SplitTunnelPolicyInclude))
	assert.Equal(t, before, st.State())
	assert.True(t, st.legacyFormat)
	st.ruleFile = path
	require.NoError(t, st.ImportLegacyPolicy(SplitTunnelPolicyInclude))
	assert.Equal(t, before, st.State())
	assert.False(t, st.legacyFormat)
}
