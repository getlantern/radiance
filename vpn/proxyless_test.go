package vpn

import (
	"testing"
	"time"

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
		assert.True(t, hedgeOpts.Disabled, "the hedge starts off, and is switched once the tunnel runs")

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
		udp := findOutbound(opts.Outbounds, ProxylessTag).Options.(*lbO.OutboundOutlineOptions).UDP
		require.NotNil(t, udp, "the UDP default applies in every mode, matching a config update's replacement")
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

// namedOutbound is an outbound that only has a tag and a type.
type namedOutbound struct {
	adapter.Outbound
	tag, typ string
}

func (o namedOutbound) Tag() string  { return o.tag }
func (o namedOutbound) Type() string { return o.typ }

// namedGroup is a group with a tag and a current member.
type namedGroup struct {
	adapter.OutboundGroup
	tag, now string
}

func (g namedGroup) Tag() string  { return g.tag }
func (g namedGroup) Type() string { return "group" }
func (g namedGroup) Now() string  { return g.now }

func TestResolveChainFollowsTheHedgeToAuto(t *testing.T) {
	ct := newConnTracker()
	hedge := namedOutbound{tag: ProxylessHedgeTag, typ: lbC.TypeHedge}
	ct.outbound = stubOutboundManager{outbounds: map[string]adapter.Outbound{
		ProxylessHedgeTag: hedge,
		AutoSelectTag:     namedGroup{tag: AutoSelectTag, now: "srv-a"},
		"srv-a":           namedOutbound{tag: "srv-a", typ: "socks"},
	}}
	outbound, _, chain := ct.resolveChain(hedge)
	assert.Equal(t, "srv-a", outbound, "auto-mode traffic stays attributed to the selected server")
	assert.Equal(t, []string{"srv-a", AutoSelectTag, ProxylessHedgeTag}, chain)
}

func TestSelectModeClosesFlowsMatchedToTheHedge(t *testing.T) {
	tun := newModeTestTunnel(t)
	ct := tun.connTracker
	hedged := joinClosable(ct, "srv-a", time.Now(), "srv-a", AutoSelectTag, ProxylessHedgeTag)
	manual := joinClosable(ct, "srv-b", time.Now(), "srv-b", ManualSelectTag)

	require.NoError(t, tun.selectMode(AutoSelectTag))
	assert.False(t, hedged.closed.Load())
	require.NoError(t, tun.selectMode(ManualSelectTag))
	assert.True(t, hedged.closed.Load(), "leaving auto mode closes the flows it routed through the hedge")
	assert.False(t, manual.closed.Load())
}

func TestProxylessStateAppliedToANewTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	assert.False(t, c.ProxylessEnabled())
	assert.ErrorIs(t, c.SetProxylessEnabled(true), ErrTunnelNotConnected, "set while no tunnel runs")
	assert.True(t, c.ProxylessEnabled(), "the state is remembered")

	hedge := &switchStub{}
	tun := newModeTestTunnel(t)
	tun.outboundMgr = stubOutboundManager{outbounds: map[string]adapter.Outbound{ProxylessHedgeTag: hedge}}
	c.mu.Lock()
	c.setTunnelLocked(tun)
	require.NoError(t, c.restoreProxylessLocked(&ProxylessOptions{Enabled: false}))
	c.mu.Unlock()
	assert.True(t, hedge.enabled, "a tunnel that starts gets the last requested state, not its build options'")
}

func TestProxylessBuildStateAppliedWhenNoneRequested(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	hedge := &switchStub{}
	tun := newModeTestTunnel(t)
	tun.outboundMgr = stubOutboundManager{outbounds: map[string]adapter.Outbound{ProxylessHedgeTag: hedge}}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setTunnelLocked(tun)
	require.NoError(t, c.restoreProxylessLocked(&ProxylessOptions{Enabled: true}))
	assert.True(t, hedge.enabled, "with no requested state, the build options' state applies")
	require.NoError(t, c.restoreProxylessLocked(nil))
}

func TestBuildOptions_NoHedgeWhenItsTagIsTaken(t *testing.T) {
	bOptions := withProxyless(t, nil)
	bOptions.Options.Outbounds = append(bOptions.Options.Outbounds, O.Outbound{Type: "direct", Tag: ProxylessHedgeTag, Options: &O.DirectOutboundOptions{}})
	bOptions.Proxyless = &ProxylessOptions{Enabled: true}
	opts, _, err := buildOptions(bOptions)
	require.NoError(t, err)
	n := 0
	for _, o := range opts.Outbounds {
		if o.Tag == ProxylessHedgeTag {
			n++
		}
	}
	assert.Equal(t, 1, n, "a saved outbound with the hedge's tag isn't duplicated")
	assert.Equal(t, AutoSelectTag, modeRoute(t, opts, AutoSelectTag), "and auto mode doesn't route to it")
}
