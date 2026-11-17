//go:build !darwin

package vpn

// checkSource is nil where there is no stale source checker.
var checkSource sourceCheckFunc
