package vpn

// shareUsageFile does nothing on Windows, where access comes from the data
// directory's ACLs, not file modes.
func shareUsageFile(string, bool) {}
