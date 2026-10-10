//go:build unix

package testutil

import (
	"os"
	"path/filepath"
	"syscall"
)

// LockBypassPort serializes tests that bind or depend on the fixed bypass proxy
// port. Packages run their tests in parallel processes, so without it one
// package's proxy can appear in the middle of another's test.
func LockBypassPort() (unlock func()) {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "radiance-bypass-port.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		panic(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		panic(err)
	}
	return func() { f.Close() }
}
