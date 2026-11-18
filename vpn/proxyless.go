package vpn

import (
	"fmt"
	"slices"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
)

const (
	// ProxylessTag is the server's proxyless (Outline smart-dialer) outbound,
	// lantern-cloud's proxylessDetourTag. It reaches sites directly, with
	// DNS and TLS evasions instead of a proxy server.
	ProxylessTag = "proxyless"
	// ProxylessHedgeTag is the hedge auto mode routes through when proxyless
	// is built: it tries ProxylessTag first and falls back to AutoSelectTag,
	// per connection, remembering which sites need the proxy.
	ProxylessHedgeTag = "proxyless-hedge"
	// proxylessUDP is the proxyless outbound's UDP transport when the server
	// sets none: Outline's QUIC prelude, a decoy Initial ahead of each QUIC
	// ClientHello, so HTTP/3 can go proxyless too.
	proxylessUDP = "quicprelude:count=1"
)

// ProxylessOptions builds the proxyless hedge into the tunnel. The hedge is
// built only when the server sent its proxyless outbound. It starts switched
// off and is switched on, after the tunnel starts, to the state last given to
// [VPNClient.SetProxylessEnabled], or to Enabled when none was. A switch made
// while the tunnel starts therefore can't be overtaken by a stale Enabled.
type ProxylessOptions struct {
	Enabled bool `json:"enabled"`
}

// WithProxylessUDP returns out with proxylessUDP as its UDP transport when out
// is the server's proxyless outbound and the server set none. Other
// outbounds, and one the server gave a UDP transport, are returned as is. The
// options are copied, so the server's config isn't changed.
func WithProxylessUDP(out O.Outbound) O.Outbound {
	if out.Tag != ProxylessTag || out.Type != lbC.TypeOutline {
		return out
	}
	opts, ok := out.Options.(*lbO.OutboundOutlineOptions)
	if !ok || opts == nil || opts.UDP != nil {
		return out
	}
	withUDP := *opts
	udp := proxylessUDP
	withUDP.UDP = &udp
	out.Options = &withUDP
	return out
}

// addProxylessHedge adds the hedge, switched off, in front of the auto group.
// It reports false, changing nothing, when the server sent no proxyless
// outbound, or when an outbound or endpoint already has the hedge's tag (a
// user server saved before the tag was reserved), which would otherwise stop
// the tunnel from starting.
func addProxylessHedge(opts *O.Options) bool {
	i := slices.IndexFunc(opts.Outbounds, func(o O.Outbound) bool {
		return o.Tag == ProxylessTag && o.Type == lbC.TypeOutline
	})
	if i < 0 {
		return false
	}
	if slices.ContainsFunc(opts.Outbounds, func(o O.Outbound) bool { return o.Tag == ProxylessHedgeTag }) ||
		slices.ContainsFunc(opts.Endpoints, func(e O.Endpoint) bool { return e.Tag == ProxylessHedgeTag }) {
		return false
	}
	opts.Outbounds = append(opts.Outbounds, O.Outbound{
		Type: lbC.TypeHedge,
		Tag:  ProxylessHedgeTag,
		Options: &lbO.HedgeOutboundOptions{
			First:    ProxylessTag,
			Then:     AutoSelectTag,
			Disabled: true,
		},
	})
	return true
}

// autoModeRule routes auto mode to target: the auto group, or the hedge in
// front of it.
func autoModeRule(target string) O.Rule {
	return O.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{
				ClashMode: AutoSelectTag,
			},
			RuleAction: O.RuleAction{
				Action: C.RuleActionTypeRoute,
				RouteOptions: O.RouteActionOptions{
					Outbound: target,
				},
			},
		},
	}
}

// SetProxylessEnabled switches the running tunnel's proxyless hedge on or
// off for new connections, without restarting the tunnel. The state is also
// remembered and applied to the next tunnel when it starts, so a switch made
// while one is starting isn't lost. With no hedge, because the tunnel isn't
// in smart routing or the server sent no proxyless outbound, there is nothing
// to switch and it returns nil.
func (c *VPNClient) SetProxylessEnabled(enabled bool) error {
	c.proxyless.Store(&enabled)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return ErrTunnelNotConnected
	}
	return c.tunnel.setProxylessEnabled(enabled)
}

// ProxylessEnabled reports the proxyless state last set with
// SetProxylessEnabled, false until it is first set.
func (c *VPNClient) ProxylessEnabled() bool {
	enabled := c.proxyless.Load()
	return enabled != nil && *enabled
}

// restoreProxylessLocked switches on a tunnel's hedge, which starts off, to
// the last requested state, or to opts' when none was requested. Caller holds
// mu.
func (c *VPNClient) restoreProxylessLocked(opts *ProxylessOptions) error {
	if c.tunnel == nil {
		return nil
	}
	if enabled := c.proxyless.Load(); enabled != nil {
		return c.tunnel.setProxylessEnabled(*enabled)
	}
	if opts == nil {
		return nil
	}
	return c.tunnel.setProxylessEnabled(opts.Enabled)
}

func (t *tunnel) setProxylessEnabled(enabled bool) error {
	out := t.liveOutbound(ProxylessHedgeTag)
	if out == nil {
		return nil
	}
	sw, ok := out.(lbA.Switchable)
	if !ok {
		return fmt.Errorf("outbound %s (%T) has no proxyless switch", ProxylessHedgeTag, out)
	}
	sw.SetEnabled(enabled)
	return nil
}
