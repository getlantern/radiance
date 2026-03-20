package vpn

import (
	"context"
	stdjson "encoding/json"
	"strings"
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
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal/testutil"
)

func setupTestSplitTunnel(t *testing.T) *SplitTunnel {
	testutil.SetPathsForTesting(t)
	s := newSplitTunnel(settings.GetString(settings.DataPathKey))
	return s
}

func TestEnableDisableIsEnabled(t *testing.T) {
	st := setupTestSplitTunnel(t)

	if assert.NoError(t, st.Disable()) {
		assert.False(t, st.IsEnabled(), "split tunnel should be disabled")
	}
	if assert.NoError(t, st.Enable()) {
		assert.True(t, st.IsEnabled(), "split tunnel should be enabled")
	}
}

func TestAddRemoveItem(t *testing.T) {
	st := setupTestSplitTunnel(t)

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
	st := setupTestSplitTunnel(t)

	require.NoError(t, st.RemoveItems(Filter{Domain: []string{"a.com"}, ProcessName: []string{"proc"}}))
	f := st.Filters()
	assert.Empty(t, f.Domain)
	assert.Empty(t, f.ProcessName)
}

func TestAddRemoveItems(t *testing.T) {
	st := setupTestSplitTunnel(t)

	items := Filter{
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

	err = st.RemoveItems(Filter{Domain: []string{"a.com"}, ProcessName: []string{"proc"}})
	require.NoError(t, err)
	f = st.Filters()
	assert.Equal(t, []string{"b.com"}, f.Domain)
	assert.Empty(t, f.ProcessName)
}

func TestFilterPersistence(t *testing.T) {
	st := setupTestSplitTunnel(t)
	require.NoError(t, st.AddItem("domain", "example.com"))

	f := st.Filters()
	assert.Equal(t, []string{"example.com"}, f.Domain)

	st = newSplitTunnel(settings.GetString(settings.DataPathKey))
	assert.NoError(t, st.loadRule())
	f = st.Filters()
	assert.Equal(t, []string{"example.com"}, f.Domain, "expected filters to persist after reloading from file")
}

func TestUpdateFilterUnsupportedType(t *testing.T) {
	st := setupTestSplitTunnel(t)
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
	st := setupTestSplitTunnel(t)
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
	require.NoError(t, st.Enable())
	require.Eventually(t, func() bool {
		return ruleSet.String() != rsStr
	}, time.Second, 50*time.Millisecond, "timed out waiting for rule reload")

	assert.True(t, rule.Match(metadata), "rule should match when split tunnel is enabled")

	rsStr = ruleSet.String()
	require.NoError(t, st.Disable())
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

func TestMigration(t *testing.T) {
	st := setupTestSplitTunnel(t)

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

func TestItemsJSON(t *testing.T) {
	st := setupTestSplitTunnel(t)

	t.Run("returns items for valid filter type", func(t *testing.T) {
		require.NoError(t, st.AddItem(TypeDomain, "example.com"))
		require.NoError(t, st.AddItem(TypeDomain, "test.org"))

		result, err := st.ItemsJSON(TypeDomain)
		require.NoError(t, err)
		assert.Equal(t, `["example.com","test.org"]`, result)
	})

	t.Run("returns empty array when no items", func(t *testing.T) {
		result, err := st.ItemsJSON(TypeDomainKeyword)
		require.NoError(t, err)
		// json.Marshal(nil slice) returns "null"; both are valid empty representations
		assert.True(t, result == `[]` || result == `null`, "expected empty JSON array, got: %s", result)
	})

	t.Run("returns error for unsupported filter type", func(t *testing.T) {
		_, err := st.ItemsJSON("unsupported")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported filter type")
	})

	t.Run("returns items for package names", func(t *testing.T) {
		require.NoError(t, st.AddItem(TypePackageName, "com.example.app"))
		result, err := st.ItemsJSON(TypePackageName)
		require.NoError(t, err)
		assert.Equal(t, `["com.example.app"]`, result)
	})
}

func TestEnabledAppsJSON(t *testing.T) {
	st := setupTestSplitTunnel(t)

	t.Run("returns empty array when no apps configured", func(t *testing.T) {
		result, err := st.EnabledAppsJSON()
		require.NoError(t, err)
		assert.Equal(t, `[]`, result)
	})

	t.Run("returns apps from current format", func(t *testing.T) {
		require.NoError(t, st.AddItem(TypePackageName, "com.example.app"))
		require.NoError(t, st.AddItem(TypeProcessPath, "/usr/bin/firefox"))

		result, err := st.EnabledAppsJSON()
		require.NoError(t, err)
		assert.Contains(t, result, "com.example.app")
		assert.Contains(t, result, "/usr/bin/firefox")
	})

	t.Run("picks up legacy camelCase keys from raw file", func(t *testing.T) {
		// Create a fresh split tunnel, then overwrite the raw file with
		// a JSON blob containing legacy camelCase keys alongside the
		// normal rule-set content. EnabledAppsJSON reads the raw file
		// for legacy keys independently of loadRule.
		st2 := setupTestSplitTunnel(t)

		// Add an app through the normal API so the current format has it
		require.NoError(t, st2.AddItem(TypePackageName, "com.current.app"))

		// Read the current file content, parse as generic map, add legacy keys
		b, err := atomicfile.ReadFile(st2.ruleFile)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, stdjson.Unmarshal(b, &raw))
		raw["packageName"] = []string{"com.legacy.app"}
		raw["processPath"] = []string{"/opt/legacy"}
		patched, err := stdjson.Marshal(raw)
		require.NoError(t, err)
		require.NoError(t, atomicfile.WriteFile(st2.ruleFile, patched, 0644))

		result, err := st2.EnabledAppsJSON()
		require.NoError(t, err)
		assert.Contains(t, result, "com.current.app")
		assert.Contains(t, result, "com.legacy.app")
		assert.Contains(t, result, "/opt/legacy")
		// Deduplication: com.current.app should appear only once
		assert.Equal(t, 1, strings.Count(result, "com.current.app"))
	})
}
