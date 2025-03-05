//go:build !ios

package app

import "runtime"

const Platform = runtime.GOOS
