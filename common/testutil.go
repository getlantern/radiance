package common

import (
	"runtime"
	"testing"

	"github.com/getlantern/radiance/vpn/ipc"
)

func SetPathsForTesting(t *testing.T) {
	if !testing.Testing() {
		panic("SetPathsForTesting should only be called in tests")
	}
	t.Helper()
	tmp := t.TempDir()
	dataPath.Store(tmp)
	logPath.Store(tmp)
	if runtime.GOOS != "windows" {
		ipc.SetSocketPath(dataPath.Load().(string))
	}
}
