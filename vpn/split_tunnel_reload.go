//go:build !novpn

package vpn

import (
	"context"
	"errors"
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
	if t.boxInstance == nil {
		err := c.failLocked(errors.New("split-tunnel router is unavailable"))
		c.mu.Unlock()
		return err
	}
	rs, ok := t.boxInstance.Router().RuleSet(splitTunnelTag)
	if !ok {
		err := c.failLocked(errors.New("split-tunnel rule set is unavailable"))
		c.mu.Unlock()
		return err
	}
	if loaded := rs.String(); loaded != previous && loaded != expected {
		c.mu.Unlock()
		return c.Restart(options)
	}
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.ctx, 2*time.Second)
	defer cancel()
	if err := awaitSplitTunnelReload(ctx, rs, expected); err != nil {
		return c.failLocked(err)
	}
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
