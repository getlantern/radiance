// Package processpath provides process-path identity across Windows aliases.
package processpath

// Normalize case-folds Windows paths and expands local absolute paths from 8.3 names when available.
// Missing paths and reparse points retain their cleaned spelling; other platforms return path unchanged.
func Normalize(path string) string {
	return normalize(path)
}
