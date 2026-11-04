//go:build !novpn

package vpn

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/getlantern/radiance/common"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
)

// ApplySplitTunnel applies a saved snapshot and closes existing flows; before must be the last applied state.
// A failed apply closes the tunnel and reports an error status.
func (c *VPNClient) ApplySplitTunnel(before SplitTunnelState, options BoxOptions) error {
	after := options.SplitTunnel
	if before.Enabled != after.Enabled || (after.Enabled && before.Policy != after.Policy) ||
		splitTunnelCaptureChanged(before, after, common.Platform) {
		return c.Restart(options)
	}
	if !after.Enabled || reflect.DeepEqual(before, after) {
		return nil
	}
	previous, err := splitTunnelRuleSummary(before)
	if err != nil {
		return c.Restart(options)
	}
	expected, err := splitTunnelRuleSummary(after)
	if err != nil {
		return c.Restart(options)
	}
	// Rule summaries can truncate or conflate selectors, so collisions require a rebuild.
	if previous == expected {
		return c.Restart(options)
	}
	return c.reloadSplitTunnel(previous, expected, options)
}

func splitTunnelRuleSummary(state SplitTunnelState) (string, error) {
	r, err := rule.NewHeadlessRule(context.Background(), O.HeadlessRule{
		Type: C.RuleTypeLogical, LogicalOptions: splitTunnelRule(state),
	})
	if err != nil {
		return "", err
	}
	return r.String(), nil
}

func (c *VPNClient) reloadSplitTunnel(previous, expected string, options BoxOptions) error {
	c.mu.Lock()
	if c.tunnel == nil || c.Status() != Connected {
		c.mu.Unlock()
		return ErrTunnelNotConnected
	}
	t := c.tunnel
	var rs adapter.RuleSet
	ok := false
	if t.boxInstance != nil {
		rs, ok = t.boxInstance.Router().RuleSet(splitTunnelTag)
	}
	c.mu.Unlock()
	if !ok {
		c.logger.Warn("Split-tunnel rule set is unavailable, restarting tunnel")
		return c.Restart(options)
	}
	if loaded := rs.String(); loaded != previous && loaded != expected {
		return c.Restart(options)
	}
	ctx, cancel := context.WithTimeout(t.ctx, 2*time.Second)
	err := awaitSplitTunnelReload(ctx, rs, expected)
	cancel()

	c.mu.Lock()
	if c.tunnel != t {
		// A replacement tunnel loads the saved rule file when it starts.
		c.mu.Unlock()
		return nil
	}
	if c.Status() != Connected {
		c.mu.Unlock()
		return ErrTunnelNotConnected
	}
	if err != nil {
		c.mu.Unlock()
		c.logger.Warn("Split-tunnel rules did not reload, restarting tunnel", "error", err)
		return c.Restart(options)
	}
	defer c.mu.Unlock()
	t.clashServer.dnsRouter.ClearCache()
	t.clashServer.connTracker.closeAllTracked()
	return nil
}

func awaitSplitTunnelReload(ctx context.Context, rs adapter.RuleSet, expected string) error {
	changed := make(chan struct{}, 1)
	callback := rs.RegisterCallback(func(adapter.RuleSet) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer rs.UnregisterCallback(callback)
	for rs.String() != expected {
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("waiting for split-tunnel rules to reload: %w", ctx.Err())
		}
	}
	return nil
}
