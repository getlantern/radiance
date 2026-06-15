//go:build !novpn

package vpn

import (
	"slices"
	"testing"

	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOptions_RejectsQUICAfterDirectRules(t *testing.T) {
	cfg := testConfig(t)
	opts, err := buildOptions(BoxOptions{
		BasePath:     t.TempDir(),
		Options:      cfg.Options,
		SmartRouting: cfg.SmartRouting,
		AdBlock:      cfg.AdBlock,
	})
	require.NoError(t, err)

	isQUICReject := func(r O.Rule) bool {
		opts := r.DefaultOptions
		return opts.RuleAction.Action == "reject" &&
			slices.Contains(opts.RawDefaultRule.Network, "udp") &&
			slices.Contains(opts.RawDefaultRule.Port, uint16(443))
	}
	isSplitTunnel := func(r O.Rule) bool {
		return slices.Contains(r.DefaultOptions.RawDefaultRule.RuleSet, splitTunnelTag)
	}
	isSelector := func(r O.Rule) bool {
		mode := r.DefaultOptions.RawDefaultRule.ClashMode
		return mode == AutoSelectTag || mode == ManualSelectTag
	}

	quicIdx, splitIdx, selectorIdx := -1, -1, -1
	for i, r := range opts.Route.Rules {
		switch {
		case isQUICReject(r):
			quicIdx = i
		case isSplitTunnel(r):
			splitIdx = i
		case isSelector(r) && selectorIdx == -1:
			selectorIdx = i
		}
	}
	require.NotEqual(t, -1, quicIdx, "expected UDP/443 reject rule in built options")
	require.NotEqual(t, -1, splitIdx, "expected split-tunnel rule in built options")
	require.NotEqual(t, -1, selectorIdx, "expected at least one selector mode rule in built options")
	assert.Greater(t, quicIdx, splitIdx, "QUIC reject must come after split-tunnel rule so split-direct domains keep QUIC")
	assert.Less(t, quicIdx, selectorIdx, "QUIC reject must come before selector rules so proxied QUIC is rejected")
}

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
