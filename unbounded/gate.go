package unbounded

import "log/slog"

// Why the widget isn't running, in the order start checks them. The snapshot
// reports one, and the manager logs each change, so a device that should be
// sharing but isn't says which condition held it back.
const (
	// BlockedDisabled: the local sharing toggle is off.
	BlockedDisabled = "disabled"
	// BlockedNotArmed: this process's backend hasn't wired the widget up
	// (LocalBackend.Start hasn't reached InitSubscription), or has closed it.
	BlockedNotArmed = "not_armed"
	// BlockedNoConfig: no server config with an unbounded section yet.
	BlockedNoConfig = "no_config"
	// BlockedFeatureOff: the server's unbounded feature flag is off.
	BlockedFeatureOff = "feature_off"
	// BlockedConfigIncomplete: the config lacks discovery or egress endpoints.
	BlockedConfigIncomplete = "config_incomplete"
	// BlockedNotStarted: every condition holds and nothing started the widget.
	BlockedNotStarted = "not_started"
)

// blockedLocked is why the widget isn't running, or "" when it is running or
// starting. m.mu must be held.
func (m *unboundedManager) blockedLocked() string {
	switch {
	case m.running || m.cancel != nil:
		return ""
	case !Enabled():
		return BlockedDisabled
	case !m.armed:
		return BlockedNotArmed
	case m.lastCfg == nil:
		return BlockedNoConfig
	case !m.lastFeatureOn:
		return BlockedFeatureOff
	case !cfgUsable(m.lastCfg):
		return BlockedConfigIncomplete
	default:
		return BlockedNotStarted
	}
}

// noteBlocked logs why the widget isn't running, once per change of reason.
// Callers run it after acting on an evaluation, without m.mu held.
func (m *unboundedManager) noteBlocked() {
	m.mu.Lock()
	reason := m.blockedLocked()
	changed := reason != m.lastBlocked
	m.lastBlocked = reason
	m.mu.Unlock()
	if changed && reason != "" {
		slog.Info("Unbounded: widget not running", "reason", reason)
	}
}
