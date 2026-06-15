//go:build novpn

package vpn

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/env"
)

// TestMain seeds a SOCKS address so the many buildOptions-based tests in this
// package don't trip socksOnlyEnforced's mandatory-address panic.
func TestMain(m *testing.M) {
	env.Set(env.SocksAddress.String(), "127.0.0.1:1080")
	os.Exit(m.Run())
}

func TestSocksOnlyEnforced_ForcesSocks(t *testing.T) {
	t.Setenv(env.SocksAddress.String(), "127.0.0.1:1080")
	require.True(t, socksOnlyEnforced())
	assert.True(t, env.GetBool(env.UseSocks), "novpn must force RADIANCE_USE_SOCKS_PROXY on")
}

func TestSocksOnlyEnforced_PanicsWithoutAddress(t *testing.T) {
	t.Setenv(env.SocksAddress.String(), "")
	assert.Panics(t, func() { socksOnlyEnforced() })
}
