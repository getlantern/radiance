package backend

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn"
)

var errSplitTunnelUpdateInProgress = errors.New("split-tunnel update already in progress")

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
	if r.splitTunnelMgr == nil {
		return errors.New("split tunneling is unavailable")
	}
	if !r.splitTunnelMu.TryLock() {
		return errSplitTunnelUpdateInProgress
	}
	defer r.splitTunnelMu.Unlock()
	if !r.connectMu.TryLock() {
		return ErrConnectInProgress
	}
	if r.vpnClient != nil {
		switch r.vpnClient.Status() {
		case vpn.Connecting, vpn.Restarting, vpn.Disconnecting:
			r.connectMu.Unlock()
			return errors.New("VPN transition in progress")
		}
	}
	before := r.splitTunnelMgr.State()
	err := update()
	after := r.splitTunnelMgr.State()
	r.connectMu.Unlock()
	if err != nil || reflect.DeepEqual(before, after) {
		return err
	}
	if r.vpnClient == nil || r.vpnClient.Status() != vpn.Connected {
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
			return nil, nil, fmt.Errorf("invalid %s: %v", settings.SplitTunnelKey, value)
		}
		enabled = &on
	}
	var policy *vpn.SplitTunnelPolicy
	if value, ok := updates[settings.SplitTunnelPolicyKey]; ok {
		text, ok := value.(string)
		p := vpn.SplitTunnelPolicy(text)
		if !ok || !p.Valid() {
			return nil, nil, fmt.Errorf("invalid %s: %v", settings.SplitTunnelPolicyKey, value)
		}
		policy = &p
	}
	return enabled, policy, nil
}
