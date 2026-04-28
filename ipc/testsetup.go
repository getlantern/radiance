package ipc

import (
	_ "unsafe" // for go:linkname
)

// this file is only used to set up testing environment

var _testing bool

//go:linkname serverTestSetup github.com/getlantern/radiance/internal/testutil.ipc_serverTestSetup
func serverTestSetup(path string) {
	setSocketPathForTesting(path)
	_testing = true
}
