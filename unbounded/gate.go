package unbounded

import "log/slog"

// Why the widget isn't running, most fundamental first: blockedLocked reports
// the first that applies. The snapshot carries it, and the manager logs each
// change, so a device that should be sharing but isn't says which condition
// held it back.
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
// starting, given whether sharing is enabled. A worker that stop has cancelled
// is neither, so it gets a reason while it tears down. m.mu must be held.
func (m *unboundedManager) blockedLocked(enabled bool) string {
	switch {
	case m.running || (m.cancel != nil && !m.stopping):
		return ""
	case !enabled:
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

// noteBlocked logs why the widget isn't running, once per change of reason,
// and logs when it runs again after a reason was logged. Callers run it after
// acting on an evaluation, without m.mu held. noteMu keeps choosing a reason
// and logging it together, so concurrent evaluations log in the order they
// chose and the last line is the current state.
func (m *unboundedManager) noteBlocked() {
	m.noteMu.Lock()
	defer m.noteMu.Unlock()
	m.mu.Lock()
	reason := m.blockedLocked(Enabled())
	changed := reason != m.lastBlocked
	m.lastBlocked = reason
	m.mu.Unlock()
	if changed {
		logBlocked(reason)
	}
}

// logBlocked logs why the widget isn't running, or, for "", that it runs
// again. Tests replace it, since they may not swap the default logger.
var logBlocked = func(reason string) {
	if reason == "" {
		slog.Info("Unbounded: widget running")
		return
	}
	slog.Info("Unbounded: widget not running", "reason", reason)
}
