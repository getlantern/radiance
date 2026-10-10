//go:build !unix

package testutil

// LockBypassPort is a no-op where flock isn't available; CI runs on Linux.
func LockBypassPort() (unlock func()) { return func() {} }
