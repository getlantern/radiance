//go:build novpn

package vpn

// ApplySplitTunnel is inert in builds without VPN support.
func (c *VPNClient) ApplySplitTunnel(_ SplitTunnelState, _ BoxOptions) error { return nil }
