//go:build android

package memmon

import "os"

// readNative reads the process RSS from /proc/self/statm. Android has no
// headroom API, so availableSupported is false and the Cap comes from the static budget.
// A read/parse failure returns footprint=0 so the Sensor falls back to goBytes.
func readNative() (footprint, available uint64, availableSupported bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, 0, false
	}
	rss, parsed := parseStatmRSS(data, os.Getpagesize())
	if !parsed {
		return 0, 0, false
	}
	return rss, 0, false
}
