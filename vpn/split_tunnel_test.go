package vpn

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
)

func setupTestSplitTunnel(t *testing.T) *SplitTunnel {
	tmp := t.TempDir()
	common.SetupDirectories(tmp, tmp)
	ruleFile := filepath.Join(common.DataPath(), splitTunnelTag+".json")
	s := &SplitTunnel{
		rule:     defaultRule(),
		ruleFile: ruleFile,
		log:      nil,
	}
	_ = s.saveToFile()
	st, err := NewSplitTunnelHandler()
	require.NoError(t, err)
	return st
}

func TestEnableDisableIsEnabled(t *testing.T) {
	st := setupTestSplitTunnel(t)

	st.Disable()
	assert.False(t, st.IsEnabled(), "expected split tunnel to be disabled")
	st.Enable()
	assert.True(t, st.IsEnabled(), "expected split tunnel to be enabled")
}

func TestAddRemoveItem(t *testing.T) {
	st := setupTestSplitTunnel(t)

	err := st.AddItem(TypeDomain, "example.com")
	require.NoError(t, err)
	f := st.Filters()
	assert.Equal(t, []string{"example.com"}, f.Domain)

	err = st.RemoveItem(TypeDomain, "example.com")
	require.NoError(t, err)
	f = st.Filters()
	assert.Empty(t, f.Domain)
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
