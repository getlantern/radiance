//go:build !windows

package vpn

import (
	"os"
	"syscall"

	"github.com/getlantern/radiance/common/fileperm"
)

// usageEUID is the user a file must belong to before shareUsageFile changes
// it. Tests replace it.
var usageEUID = os.Geteuid

// shareUsageFile gives path fileperm.File, creating it empty first if create
// is set. On macOS the root network extension and the app both open the
// usage file and its lock, the app while the VPN is off, and a file the root
// extension created in another mode, or under a restrictive umask, is one the
// app can't open.
//
// It opens path with O_NOFOLLOW, so the kernel refuses a link in its place,
// and changes the file through that handle, so a file swapped in after the
// check is never changed. It changes only a regular file with one link that
// this process owns: root then never widens another file, and the process
// that left the mode wrong can fix it.
func shareUsageFile(path string, create bool) {
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, fileperm.File)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() == fileperm.File {
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || int(stat.Uid) != usageEUID() {
		return
	}
	_ = f.Chmod(fileperm.File)
}
