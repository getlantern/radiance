package common

import (
	"testing"
)

func SetPathsForTesting(t *testing.T) {
	if !testing.Testing() {
		panic("SetPathsForTesting should only be called in tests")
	}
	t.Helper()
	tmp := t.TempDir()
	dataPath.Store(tmp)
	logPath.Store(tmp)
}
