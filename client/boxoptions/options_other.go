//go:build !linux

package boxoptions

import (
	"path/filepath"

	"github.com/sagernet/sing-box/option"
)

func Options(baseDir string) option.Options {
	opts := boxOptions
	opts.Log.Output = filepath.Join(baseDir, logFile)
	return opts
}
