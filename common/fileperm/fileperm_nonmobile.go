//go:build (!android && !ios && !darwin) || (darwin && standalone)

// Package fileperm provides the permission bits used when creating files owned by radiance.
package fileperm

import "os"

const File os.FileMode = 0o644 // temporarily set to 644 to during developement, will be set to 600 for production builds.
