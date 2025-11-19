package vpn

import (
	stdjson "encoding/json"
	"os"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
)

func setupTestAdBlocker(t *testing.T) *AdBlocker {
	t.Helper()
	common.SetPathsForTesting(t)

	a, err := NewAdBlocker()
	require.NoError(t, err, "NewAdBlockerHandler")
	require.NotEmpty(t, a.ruleFile, "ruleFile must be set")
	return a
}

func loadAdblockRuleSet(t *testing.T, a *AdBlocker) adblockRuleSet {
	t.Helper()

	content, err := os.ReadFile(a.ruleFile)
	require.NoError(t, err, "read rule file")

	var rs adblockRuleSet
	require.NoError(t, stdjson.Unmarshal(content, &rs), "unmarshal rule file")
	return rs
}

func TestAdBlockerInitialState(t *testing.T) {
	a := setupTestAdBlocker(t)

	// Default should be disabled
	assert.False(t, a.IsEnabled(), "adblock should be disabled by default")
	assert.Equal(t, C.LogicalTypeAnd, a.mode, "default mode should be AND")

	// Load ad block rule set
	rs := loadAdblockRuleSet(t, a)

	require.Equal(t, 3, rs.Version, "version should be 3")
	require.Len(t, rs.Rules, 2, "should have two rules (logical + rule_set)")

	// First rule: logical gate
	logicalRule := rs.Rules[0]
	assert.Equal(t, "logical", logicalRule.Type, "first rule should be logical")
	require.NotNil(t, logicalRule.Logical, "logical must not be nil")
	assert.Equal(t, C.LogicalTypeAnd, logicalRule.Logical.Mode, "logical mode should be AND by default")

	require.Len(t, logicalRule.Logical.Rules, 2, "logical should have 2 inner rules")

	// Inner rule where domain == disable.rule
	r1 := logicalRule.Logical.Rules[0]
	assert.Equal(t, "default", r1.Type)
	assert.Equal(t, []string{"disable.rule"}, []string(r1.DefaultOptions.Domain))
	assert.False(t, r1.DefaultOptions.Invert, "should not invert")

	// Inner rule where domain == disable.rule, invert == true
	r2 := logicalRule.Logical.Rules[1]
	assert.Equal(t, "default", r2.Type)
	assert.Equal(t, []string{"disable.rule"}, []string(r2.DefaultOptions.Domain))
	assert.True(t, r2.DefaultOptions.Invert, "should invert")

	// Second rule: rule_set -> adblock-list
	rsRule := rs.Rules[1]
	assert.Equal(t, "rule_set", rsRule.Type, "second rule should be rule_set")
	assert.Equal(t, []string{adBlockListTag}, rsRule.RuleSet, "rule_set should reference adblock list tag")
}

func TestAdBlockerEnableDisable(t *testing.T) {
	a := setupTestAdBlocker(t)

	// Enable
	require.NoError(t, a.SetEnabled(true), "enable adblock")
	assert.True(t, a.IsEnabled(), "adblock should be enabled")
	assert.Equal(t, C.LogicalTypeOr, a.mode, "mode should be OR when enabled")

	rs := loadAdblockRuleSet(t, a)
	require.NotNil(t, rs.Rules[0].Logical)
	assert.Equal(t, C.LogicalTypeOr, rs.Rules[0].Logical.Mode, "file mode should be OR when enabled")

	// Disable
	require.NoError(t, a.SetEnabled(false), "disable adblock")
	assert.False(t, a.IsEnabled(), "adblock should be disabled")
	assert.Equal(t, C.LogicalTypeAnd, a.mode, "mode should be AND when disabled")

	rs = loadAdblockRuleSet(t, a)
	require.NotNil(t, rs.Rules[0].Logical)
	assert.Equal(t, C.LogicalTypeAnd, rs.Rules[0].Logical.Mode, "file mode should be AND when disabled")
}

func TestAdBlockerPersistence(t *testing.T) {
	a := setupTestAdBlocker(t)

	require.NoError(t, a.SetEnabled(true), "enable adblock")
	assert.True(t, a.IsEnabled(), "adblock should be enabled before reload")

	b, err := NewAdBlocker()
	require.NoError(t, err, "NewAdBlocker (reload)")

	assert.True(t, b.IsEnabled(), "adblock should stay enabled after reload")
	assert.Equal(t, C.LogicalTypeOr, b.mode, "mode should still be OR after reload")

	rs := loadAdblockRuleSet(t, b)
	require.NotNil(t, rs.Rules[0].Logical)
	assert.Equal(t, C.LogicalTypeOr, rs.Rules[0].Logical.Mode, "file mode should stay OR after reload")
}
