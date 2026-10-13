package settings

// SetRuntimePaths sets diagnostics paths in memory without loading or writing settings.
func SetRuntimePaths(dataDir, logDir string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.k.Set(DataPathKey.String(), dataDir); err != nil {
		return err
	}
	return k.k.Set(LogPathKey.String(), logDir)
}
