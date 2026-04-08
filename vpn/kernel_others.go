//go:build !linux && !android

package vpn

// kernelVersion returns an empty string on non-Linux platforms where kernel
// version detection is not needed for TUN stack selection.
func kernelVersion() string { return "" }
