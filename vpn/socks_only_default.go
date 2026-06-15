//go:build !novpn

package vpn

func socksOnlyEnforced() bool { return false }
