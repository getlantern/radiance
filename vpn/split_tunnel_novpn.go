//go:build novpn

package vpn

import "log/slog"

// SplitTunnel is inert in builds without VPN support.
type SplitTunnel struct{}

func NewSplitTunnelHandler(_ string, _ *slog.Logger) (*SplitTunnel, error) {
	return &SplitTunnel{}, nil
}

func (s *SplitTunnel) IsEnabled() bool { return false }

func (s *SplitTunnel) AddItems(_ SplitTunnelFilter) error { return nil }

func (s *SplitTunnel) RemoveItems(_ SplitTunnelFilter) error { return nil }

// State returns disabled split-tunnel state in builds without VPN support.
func (s *SplitTunnel) State() SplitTunnelState {
	return SplitTunnelState{Policy: SplitTunnelPolicyExclude}
}

// SwitchPolicy is inert in builds without VPN support.
func (s *SplitTunnel) SwitchPolicy(_ SplitTunnelPolicy) error { return nil }

// ClearApps is inert in builds without VPN support.
func (s *SplitTunnel) ClearApps() error { return nil }

// ImportLegacyPolicy is inert in builds without VPN support.
func (s *SplitTunnel) ImportLegacyPolicy(_ SplitTunnelPolicy) error { return nil }

// Configure is inert in builds without VPN support.
func (s *SplitTunnel) Configure(_ *bool, _ *SplitTunnelPolicy) error { return nil }
