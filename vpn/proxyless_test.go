package vpn

import (
	"testing"

	"github.com/sagernet/sing-box/adapter"
	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	rlog "github.com/getlantern/radiance/log"
)

// proxylessOutbound mimics lantern-cloud's proxyless outbound, with udp set
// when udp is non-nil.
func proxylessOutbound(udp *string) O.Outbound {
	return O.Outbound{
		Type: lbC.TypeOutline,
		Tag:  ProxylessTag,
		Options: &lbO.OutboundOutlineOptions{
			Domains:     []string{"cdn.jsdelivr.net"},
			TestTimeout: "5s",
			TLS:         []string{"", "split:1"},
			UDP:         udp,
		},
	}
}

// withProxyless returns the test config's options plus the server's
// proxyless outbound, listed as non-selectable the way the server sends it.
func withProxyless(t *testing.T, udp *string) BoxOptions {
	cfg := testConfig(t)
	cfg.Options.Outbounds = append(cfg.Options.Outbounds, proxylessOutbound(udp))
	return BoxOptions{
		BasePath:               t.TempDir(),
		Options:                cfg.Options,
		NonSelectableOutbounds: []string{ProxylessTag},
	}
}

// modeRoute returns the outbound the clash-mode rule for mode routes to.
func modeRoute(t *testing.T, opts O.Options, mode string) string {
	t.Helper()
	for _, r := range opts.Route.Rules {
		if r.DefaultOptions.RawDefaultRule.ClashMode == mode {
			return r.DefaultOptions.RuleAction.RouteOptions.Outbound
		}
	}
	t.Fatalf("no rule for clash mode %q", mode)
	return ""
}

func TestBuildOptions_ProxylessHedge(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		bOptions := withProxyless(t, nil)
		bOptions.Proxyless = &ProxylessOptions{Enabled: enabled}
		opts, _, err := buildOptions(bOptions)
		require.NoError(t, err)

		hedge := findOutbound(opts.Outbounds, ProxylessHedgeTag)
		require.NotNil(t, hedge, "smart routing with the server's proxyless outbound builds the hedge")
		hedgeOpts := hedge.Options.(*lbO.HedgeOutboundOptions)
		assert.Equal(t, ProxylessTag, hedgeOpts.First)
		assert.Equal(t, AutoSelectTag, hedgeOpts.Then)
		assert.Equal(t, !enabled, hedgeOpts.Disabled, "the hedge starts switched as the flag and setting say")

		assert.Equal(t, ProxylessHedgeTag, modeRoute(t, opts, AutoSelectTag), "auto mode goes through the hedge")
		assert.Equal(t, ManualSelectTag, modeRoute(t, opts, ManualSelectTag), "manual mode never goes proxyless")

		out := findOutbound(opts.Outbounds, ProxylessTag)
		require.NotNil(t, out)
		udp := out.Options.(*lbO.OutboundOutlineOptions).UDP
		require.NotNil(t, udp, "the proxyless outbound gets UDP so QUIC can go proxyless")
		assert.Equal(t, proxylessUDP, *udp)

		auto := findOutbound(opts.Outbounds, AutoSelectTag).Options.(*lbO.MutableAutoSelectOutboundOptions)
		assert.NotContains(t, auto.Outbounds, ProxylessTag, "proxyless is never an auto member")
		assert.NotContains(t, auto.Outbounds, ProxylessHedgeTag, "nor is the hedge")

		served := findOutbound(bOptions.Options.Outbounds, ProxylessTag).Options.(*lbO.OutboundOutlineOptions)
		assert.Nil(t, served.UDP, "the server's config is not modified")
	}
}

func TestBuildOptions_ProxylessKeepsServerUDP(t *testing.T) {
	plain := ""
	bOptions := withProxyless(t, &plain)
	bOptions.Proxyless = &ProxylessOptions{Enabled: true}
	opts, _, err := buildOptions(bOptions)
	require.NoError(t, err)
	udp := findOutbound(opts.Outbounds, ProxylessTag).Options.(*lbO.OutboundOutlineOptions).UDP
	require.NotNil(t, udp)
	assert.Equal(t, "", *udp, "a UDP transport the server chose is kept")
}

func TestBuildOptions_NoProxylessHedge(t *testing.T) {
	t.Run("full tunnel", func(t *testing.T) {
		opts, _, err := buildOptions(withProxyless(t, nil))
		require.NoError(t, err)
		assert.Nil(t, findOutbound(opts.Outbounds, ProxylessHedgeTag), "full tunnel never builds the hedge")
		assert.Equal(t, AutoSelectTag, modeRoute(t, opts, AutoSelectTag))
	})
	t.Run("no proxyless outbound from the server", func(t *testing.T) {
		cfg := testConfig(t)
		opts, _, err := buildOptions(BoxOptions{
			BasePath:  t.TempDir(),
			Options:   cfg.Options,
			Proxyless: &ProxylessOptions{Enabled: true},
		})
		require.NoError(t, err)
		assert.Nil(t, findOutbound(opts.Outbounds, ProxylessHedgeTag))
		assert.Equal(t, AutoSelectTag, modeRoute(t, opts, AutoSelectTag))
	})
}

func TestWithProxylessUDP(t *testing.T) {
	served := proxylessOutbound(nil)
	got := WithProxylessUDP(served)
	require.NotNil(t, got.Options.(*lbO.OutboundOutlineOptions).UDP)
	assert.Nil(t, served.Options.(*lbO.OutboundOutlineOptions).UDP, "the input is copied, not modified")

	other := O.Outbound{Type: lbC.TypeOutline, Tag: "not-proxyless", Options: &lbO.OutboundOutlineOptions{}}
	assert.Nil(t, WithProxylessUDP(other).Options.(*lbO.OutboundOutlineOptions).UDP, "only the proxyless outbound")
}

// switchStub is an outbound with the hedge's runtime switch.
type switchStub struct {
	adapter.Outbound
	enabled bool
}

func (s *switchStub) SetEnabled(enabled bool) { s.enabled = enabled }
func (s *switchStub) Enabled() bool           { return s.enabled }

func TestSetProxylessEnabled(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	assert.ErrorIs(t, c.SetProxylessEnabled(true), ErrTunnelNotConnected)

	tun := newModeTestTunnel(t)
	c.tunnel = tun
	tun.outboundMgr = stubOutboundManager{outbounds: map[string]adapter.Outbound{}}
	assert.NoError(t, c.SetProxylessEnabled(true), "a tunnel without the hedge has nothing to switch")

	hedge := &switchStub{}
	tun.outboundMgr = stubOutboundManager{outbounds: map[string]adapter.Outbound{ProxylessHedgeTag: hedge}}
	require.NoError(t, c.SetProxylessEnabled(true))
	assert.True(t, hedge.enabled)
	require.NoError(t, c.SetProxylessEnabled(false))
	assert.False(t, hedge.enabled)

	tun.outboundMgr = stubOutboundManager{outbounds: map[string]adapter.Outbound{ProxylessHedgeTag: stubGroup{}}}
	assert.Error(t, c.SetProxylessEnabled(true), "an outbound under the hedge's tag without a switch is an error")
}
