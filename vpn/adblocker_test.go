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

func loadAdblockRuleSet(t *testing.T, a *AdBlocker) adblockRuleSetFile {
	t.Helper()

	content, err := os.ReadFile(a.ruleFile)
	require.NoError(t, err, "read rule file")

	var rs adblockRuleSetFile
	require.NoError(t, stdjson.Unmarshal(content, &rs), "unmarshal rule file")
	return rs
}

func TestAdBlockerInitialState(t *testing.T) {
	a := setupTestAdBlocker(t)

	// Default should be disabled
	assert.False(t, a.IsEnabled(), "adblock should be disabled by default")
	assert.Equal(t, C.LogicalTypeAnd, a.mode, "default mode should be AND")

	rs := loadAdblockRuleSet(t, a)

	require.Equal(t, 3, rs.Version, "version should be 3")
	require.Len(t, rs.Rules, 1, "should have one top-level rule (outer logical)")

	outer := rs.Rules[0]
	assert.Equal(t, "logical", outer.Type, "top-level rule should be logical")
	assert.Equal(t, C.LogicalTypeAnd, outer.Mode, "outer mode should be AND by default")
	require.Len(t, outer.Rules, 2, "outer logical should have 2 inner rules (disable gate + ruleset ref)")

	disableGate := outer.Rules[0]
	assert.Equal(t, "logical", disableGate.Type, "first inner rule should be logical (disable gate)")
	assert.Equal(t, C.LogicalTypeAnd, disableGate.Mode, "disable gate mode should be AND")
	require.Len(t, disableGate.Rules, 2, "disable gate should have 2 inner default rules")

	r1 := disableGate.Rules[0]
	assert.Equal(t, "default", r1.Type)
	assert.Equal(t, []string{"disable.rule"}, r1.Domain)
	assert.False(t, r1.Invert, "should not invert")

	r2 := disableGate.Rules[1]
	assert.Equal(t, "default", r2.Type)
	assert.Equal(t, []string{"disable.rule"}, r2.Domain)
	assert.True(t, r2.Invert, "should invert")

	ref := outer.Rules[1]
	assert.Equal(t, "default", ref.Type)
}

func TestAdBlockerEnableDisable(t *testing.T) {
	a := setupTestAdBlocker(t)

	// Enable
	require.NoError(t, a.SetEnabled(true), "enable adblock")
	assert.True(t, a.IsEnabled(), "adblock should be enabled")
	assert.Equal(t, C.LogicalTypeOr, a.mode, "mode should be OR when enabled")

	rs := loadAdblockRuleSet(t, a)
	require.Len(t, rs.Rules, 1)
	assert.Equal(t, C.LogicalTypeOr, rs.Rules[0].Mode, "file mode should be OR when enabled")

	// Disable
	require.NoError(t, a.SetEnabled(false), "disable adblock")
	assert.False(t, a.IsEnabled(), "adblock should be disabled")
	assert.Equal(t, C.LogicalTypeAnd, a.mode, "mode should be AND when disabled")

	rs = loadAdblockRuleSet(t, a)
	require.Len(t, rs.Rules, 1)
	assert.Equal(t, C.LogicalTypeAnd, rs.Rules[0].Mode, "file mode should be AND when disabled")
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
	require.Len(t, rs.Rules, 1)
	assert.Equal(t, C.LogicalTypeOr, rs.Rules[0].Mode, "file mode should stay OR after reload")
}
