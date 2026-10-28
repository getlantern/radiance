//go:build !novpn

package vpn

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rlog "github.com/getlantern/radiance/log"
)

type splitTestDNS struct {
	adapter.DNSRouter
	cleared bool
}

func (d *splitTestDNS) ClearCache() { d.cleared = true }

func splitReloadClient(t *testing.T, watch bool, filters ...SplitTunnelFilter) (*VPNClient, *SplitTunnel, *splitTestDNS) {
	t.Helper()
	dir := t.TempDir()
	manager, err := NewSplitTunnelHandler(dir, rlog.NoOpLogger())
	require.NoError(t, err)
	require.NoError(t, manager.SetPolicy(SplitTunnelPolicyInclude))
	require.NoError(t, manager.SetEnabled(true))
	for _, selected := range filters {
		require.NoError(t, manager.AddItems(selected))
	}
	ctx, cancel := context.WithCancel(box.Context(t.Context()))
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: O.Options{
		Log: &O.LogOptions{Disabled: true}, Route: &O.RouteOptions{RuleSet: splitTunnelRuleSet(dir)},
	}})
	require.NoError(t, err)
	if watch {
		rs, ok := instance.Router().RuleSet(splitTunnelTag)
		require.True(t, ok)
		require.NoError(t, rs.StartContext(ctx, nil))
	}
	dns := &splitTestDNS{}
	cs := newTestClashServer(t)
	cs.dnsRouter = dns
	c := NewVPNClient(dir, rlog.NoOpLogger(), nil)
	c.tunnel = &tunnel{
		ctx: ctx, cancel: cancel, boxInstance: instance, clashServer: cs,
		closers: []io.Closer{instance},
	}
	c.status.Store(Connected)
	t.Cleanup(func() { require.NoError(t, c.Disconnect()) })
	return c, manager, dns
}

func TestReloadSplitTunnelClosesExistingFlows(t *testing.T) {
	c, manager, dns := splitReloadClient(t, true)
	tracker := c.tunnel.clashServer.connTracker
	for _, outbound := range []string{"direct", AutoSelectTag} {
		joinClosable(tracker, outbound, time.Now())
	}
	before := manager.State()
	require.NoError(t, manager.AddItem(TypeProcessName, "selected-app"))
	require.NoError(t, c.ApplySplitTunnel(before, BoxOptions{SplitTunnel: manager.State()}))
	count, _ := tracker.activeStats()
	assert.Zero(t, count)
	assert.True(t, dns.cleared)
	assert.Equal(t, Connected, c.Status())
	assert.NotNil(t, c.tunnel)
	// An already-loaded snapshot must not wait for another file notification.
	require.NoError(t, c.ApplySplitTunnel(before, BoxOptions{SplitTunnel: manager.State()}))
}

func TestReloadSplitTunnelFailureClosesTunnel(t *testing.T) {
	c, manager, dns := splitReloadClient(t, false)
	before := manager.State()
	require.NoError(t, manager.AddItem(TypeProcessName, "selected-app"))
	c.tunnel.cancel()
	err := c.ApplySplitTunnel(before, BoxOptions{SplitTunnel: manager.State()})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ErrorStatus, c.Status())
	assert.Nil(t, c.tunnel)
	assert.False(t, dns.cleared)
	loaded, err := NewSplitTunnelHandler(filepath.Dir(manager.ruleFile), rlog.NoOpLogger())
	require.NoError(t, err)
	assert.Equal(t, manager.State(), loaded.State())
}

func TestAwaitSplitTunnelReloadDeadline(t *testing.T) {
	c, _, _ := splitReloadClient(t, false)
	rs, ok := c.tunnel.boxInstance.Router().RuleSet(splitTunnelTag)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, awaitSplitTunnelReload(ctx, rs, "not loaded"), context.DeadlineExceeded)
}

func TestReloadSplitTunnelUnavailable(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	before := SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude}
	options := BoxOptions{SplitTunnel: SplitTunnelState{
		Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{Domain: []string{"selected.test"}},
	}}
	require.ErrorIs(t, c.ApplySplitTunnel(before, options), ErrTunnelNotConnected)
	c.status.Store(Connected)
	c.tunnel = &tunnel{}
	require.ErrorContains(t, c.ApplySplitTunnel(before, options), "router is unavailable")
	assert.Equal(t, ErrorStatus, c.Status())
	assert.Nil(t, c.tunnel)
}

func TestApplySplitTunnelRebuildsAmbiguousSummaries(t *testing.T) {
	for _, tt := range []struct {
		name          string
		before, after SplitTunnelFilter
	}{
		{"truncated domains", SplitTunnelFilter{Domain: []string{"a.test", "b.test", "c.test", "d.test"}},
			SplitTunnelFilter{Domain: []string{"a.test", "b.test", "c.test", "e.test"}}},
		{"ambiguous processes", SplitTunnelFilter{ProcessName: []string{"a b", "c"}},
			SplitTunnelFilter{ProcessName: []string{"a", "b c"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, manager, _ := splitReloadClient(t, false, tt.before)
			before := manager.State()
			require.NoError(t, manager.RemoveItems(tt.before))
			require.NoError(t, manager.AddItems(tt.after))
			after := manager.State()
			previous, err := splitTunnelRuleSummary(before)
			require.NoError(t, err)
			expected, err := splitTunnelRuleSummary(after)
			require.NoError(t, err)
			require.Equal(t, previous, expected)
			err = c.ApplySplitTunnel(before, BoxOptions{SplitTunnel: after})
			require.ErrorContains(t, err, "failed to build options")
			assert.Equal(t, ErrorStatus, c.Status())
			assert.Nil(t, c.tunnel)
		})
	}
}

func TestRestartFailureReleasesOldTunnel(t *testing.T) {
	for _, platform := range []bool{false, true} {
		for _, restartErr := range []error{nil, errors.New("restart failed")} {
			if !platform && restartErr != nil {
				continue
			}
			var p PlatformInterface
			if platform {
				p = &stubPlatform{restartErr: restartErr}
			}
			c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), p)
			closed := false
			c.tunnel = &tunnel{closers: []io.Closer{closerFunc(func() error { closed = true; return nil })}}
			c.status.Store(Connected)
			err := c.Restart(BoxOptions{})
			require.Error(t, err)
			if restartErr != nil {
				assert.ErrorIs(t, err, restartErr)
			}
			assert.Equal(t, ErrorStatus, c.Status())
			assert.Nil(t, c.tunnel)
			assert.True(t, closed)
		}
	}
}
