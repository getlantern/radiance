//go:build !(ios && cgo) && !android

package memmon

func readNative() (footprint, available uint64, availableSupported bool) { return 0, 0, false }
