//go:build !windows

package vpn

// removeOrphanedTUNAdapters is a no-op off Windows: the orphaned-devnode failure
// it clears is a Windows PnP condition with no analogue on other platforms.
func removeOrphanedTUNAdapters([]string) (int, error) {
	return 0, nil
}
