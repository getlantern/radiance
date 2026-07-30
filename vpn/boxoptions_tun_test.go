//go:build !novpn

package vpn

import (
	"testing"

	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBaseOpts_TunIPv6Address(t *testing.T) {
	opts := baseOpts(t.TempDir())
	require.NotEmpty(t, opts.Inbounds, "expected inbounds in baseOpts output")

	var tunOpts *O.TunInboundOptions
	for _, in := range opts.Inbounds {
		if in.Type == "tun" {
			var ok bool
			tunOpts, ok = in.Options.(*O.TunInboundOptions)
			require.True(t, ok, "expected *TunInboundOptions for tun inbound")
			break
		}
	}
	require.NotNil(t, tunOpts, "expected a tun inbound")
	require.NotEmpty(t, tunOpts.Address, "expected at least the v4 TUN address")
	assert.Equal(t, "10.10.1.1/30", tunOpts.Address[0].String(), "first TUN address should be the v4 prefix")

	if hasGlobalIPv6() {
		require.Len(t, tunOpts.Address, 2, "expected v4 + v6 ULA on TUN when system has global v6")
		assert.Equal(t, "fdfe:dcba:9876::1/126", tunOpts.Address[1].String(),
			"v6 ULA should be appended after the v4 address")
	} else {
		require.Len(t, tunOpts.Address, 1, "expected v4-only TUN when system has no global v6")
	}
}

func TestKernelBelow(t *testing.T) {
	tests := []struct {
		name string
		v    string
		min  string
		want bool
	}{
		{"below major", "4.19.0", "5.10", true},
		{"below minor", "5.4.0", "5.10", true},
		{"equal", "5.10.0", "5.10", false},
		{"above minor", "5.15.0", "5.10", false},
		{"above major", "6.1.0", "5.10", false},
		{"android suffix", "4.19.0-android13", "5.10", true},
		{"android suffix above", "5.15.0-android13", "5.10", false},
		{"empty version", "", "5.10", false},
		{"empty min", "5.10.0", "", false},
		{"both empty", "", "", false},
		{"invalid version", "not-a-version", "5.10", false},
		{"only major", "5", "5.10", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kernelBelow(tt.v, tt.min))
		})
	}
}
