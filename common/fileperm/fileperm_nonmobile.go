//go:build (!android && !ios && !darwin) || (darwin && standalone)

// Package fileperm provides the permission bits used when creating files owned by radiance.
package fileperm

import "os"

const File os.FileMode = 0o644 // temporarily set to 644 until full release, after which it will be set to 600.
