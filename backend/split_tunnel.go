package backend

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn"
)

// ErrVPNBusy reports that a split-tunnel change or VPN restart was refused because other VPN
// work was in progress. The refused operation had no effect and is retryable once that work finishes.
var ErrVPNBusy = errors.New("VPN is busy with another change")

// ErrSplitTunnelUnavailable reports that the backend has no split-tunnel manager.
// It is permanent for the backend's lifetime.
var ErrSplitTunnelUnavailable = errors.New("split tunneling is unavailable")

// splitTunnelLockTimeout bounds how long a change waits behind a split-tunnel apply or
// restart, which can include a full platform VPN restart.
var splitTunnelLockTimeout = 30 * time.Second

// waitMutex is a mutex whose lock can time out. The zero value is unlocked.
type waitMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *waitMutex) lock(timeout time.Duration) bool {
	m.once.Do(func() { m.ch = make(chan struct{}, 1) })
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case m.ch <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func (m *waitMutex) unlock() { <-m.ch }

// Settings returns preferences with the persisted split-tunnel state projected into its legacy keys.
func (r *LocalBackend) Settings() settings.Settings {
	s := settings.GetAll()
	if r.splitTunnelMgr != nil {
		state := r.splitTunnelMgr.State()
		s[settings.SplitTunnelKey] = state.Enabled
		s[settings.SplitTunnelPolicyKey] = string(state.Policy)
	}
	return s
}

// SplitTunnelState returns the persisted policy and a copy of its selections.
func (r *LocalBackend) SplitTunnelState() vpn.SplitTunnelState {
	if r.splitTunnelMgr == nil {
		return vpn.SplitTunnelState{Policy: vpn.SplitTunnelPolicyExclude}
	}
	return r.splitTunnelMgr.State()
}

// SwitchSplitTunnelPolicy changes policy and clears selections only when the policy changes.
func (r *LocalBackend) SwitchSplitTunnelPolicy(policy vpn.SplitTunnelPolicy) error {
	return r.updateSplitTunnel(func() error { return r.splitTunnelMgr.SwitchPolicy(policy) })
}

// ClearSplitTunnelApps removes every app selector, preserving website selections.
func (r *LocalBackend) ClearSplitTunnelApps() error {
	return r.updateSplitTunnel(func() error { return r.splitTunnelMgr.ClearApps() })
}

// SplitTunnelFilters returns an independent copy of the persisted selections.
func (r *LocalBackend) SplitTunnelFilters() vpn.SplitTunnelFilter {
	return r.SplitTunnelState().Filters
}

// AddSplitTunnelItems saves selections and applies them to a connected tunnel.
func (r *LocalBackend) AddSplitTunnelItems(items vpn.SplitTunnelFilter) error {
	return r.updateSplitTunnel(func() error { return r.splitTunnelMgr.AddItems(items) })
}

// RemoveSplitTunnelItems saves removals and applies them to a connected tunnel.
func (r *LocalBackend) RemoveSplitTunnelItems(items vpn.SplitTunnelFilter) error {
	return r.updateSplitTunnel(func() error { return r.splitTunnelMgr.RemoveItems(items) })
}

func (r *LocalBackend) updateSplitTunnel(update func() error) error {
	return r.saveSplitTunnel(update, true)
}

// saveSplitTunnel persists update. With apply false it leaves a connected
// tunnel on the old rules, for a caller that restarts it next.
func (r *LocalBackend) saveSplitTunnel(update func() error, apply bool) error {
	if r.splitTunnelMgr == nil {
		return ErrSplitTunnelUnavailable
	}
	if !r.splitTunnelMu.lock(splitTunnelLockTimeout) {
		return ErrVPNBusy
	}
	defer r.splitTunnelMu.unlock()
	if !r.connectMu.TryLock() {
		return fmt.Errorf("%w: %w", ErrVPNBusy, ErrConnectInProgress)
	}
	if r.vpnClient != nil {
		switch r.vpnClient.Status() {
		case vpn.Connecting, vpn.Restarting, vpn.Disconnecting:
			r.connectMu.Unlock()
			return ErrVPNBusy
		}
	}
	before := r.splitTunnelMgr.State()
	err := update()
	after := r.splitTunnelMgr.State()
	r.connectMu.Unlock()
	if err != nil || reflect.DeepEqual(before, after) {
		return err
	}
	// Older builds read split-tunnel state from settings, not the rule file.
	if err := settings.Patch(settings.Settings{
		settings.SplitTunnelKey:       after.Enabled,
		settings.SplitTunnelPolicyKey: string(after.Policy),
	}); err != nil {
		slog.Warn("Failed to mirror split-tunnel state into settings", "error", err)
	}
	if !apply || r.vpnClient == nil || r.vpnClient.Status() != vpn.Connected {
		return nil
	}
	err = r.vpnClient.ApplySplitTunnel(before, r.getBoxOptions())
	if errors.Is(err, vpn.ErrTunnelNotConnected) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("split-tunnel settings saved but could not be applied: %w", err)
	}
	return nil
}

func splitTunnelSettings(updates settings.Settings) (*bool, *vpn.SplitTunnelPolicy, error) {
	var enabled *bool
	if value, ok := updates[settings.SplitTunnelKey]; ok {
		on, ok := value.(bool)
		if !ok {
			return nil, nil, fmt.Errorf("invalid %s: want a bool, got %T", settings.SplitTunnelKey, value)
		}
		enabled = &on
	}
	var policy *vpn.SplitTunnelPolicy
	if value, ok := updates[settings.SplitTunnelPolicyKey]; ok {
		text, ok := value.(string)
		p := vpn.SplitTunnelPolicy(text)
		if !ok || !p.Valid() {
			return nil, nil, fmt.Errorf("invalid %s: want %q or %q (got %T)",
				settings.SplitTunnelPolicyKey, vpn.SplitTunnelPolicyExclude, vpn.SplitTunnelPolicyInclude, value)
		}
		policy = &p
	}
	return enabled, policy, nil
}
