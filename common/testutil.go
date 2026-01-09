package common

import (
	"runtime"
	"testing"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn/ipc"
)

func SetPathsForTesting(t *testing.T) {
	if !testing.Testing() {
		panic("SetPathsForTesting should only be called in tests")
	}
	t.Helper()
	tmp := t.TempDir()
	settings.Set(settings.DataPathKey, tmp)
	settings.Set(settings.LogPathKey, tmp)
	if runtime.GOOS != "windows" {
		ipc.SetSocketPath(settings.GetString(settings.DataPathKey))
	}
}
