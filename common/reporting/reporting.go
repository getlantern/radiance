// Package reporting provides slog-backed hooks for crash and panic reporting.
package reporting

import (
	"context"
	"log/slog"
	"runtime/debug"

	"github.com/getlantern/lantern-box/connectiondiag"
	"github.com/getlantern/radiance/log"
)

// Init enables local connection diagnostics and logs the build version.
func Init(version string) {
	connectiondiag.Enable(false)
	slog.Info("reporting initialized", "version", version)
}

// PanicListener logs msg with the captured stack at [log.LevelPanic].
func PanicListener(msg string) {
	slog.Log(context.Background(), log.LevelPanic, msg,
		"stack", string(debug.Stack()),
	)
}
