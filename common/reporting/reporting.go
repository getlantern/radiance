// Package reporting provides hooks for crash/panic reporting. Sentry has
// been removed; the implementation is now slog-only. The package keeps its
// previous API (Init, PanicListener) so callers don't need to change.
package reporting

import (
	"context"
	"log/slog"
	"runtime/debug"

	"github.com/getlantern/radiance/log"
)

// Init was previously where the Sentry SDK got configured. It is kept for
// API compatibility — callers can still pass the build version — but is
// now a no-op beyond logging the version.
func Init(version string) {
	slog.Info("reporting initialized (sentry disabled)", "version", version)
}

// PanicListener logs a panic message and a captured stack trace at the
// dedicated panic level. It is passed as a callback to libraries that need
// a panic notification hook.
func PanicListener(msg string) {
	slog.Log(context.Background(), log.LevelPanic, msg,
		"stack", string(debug.Stack()),
	)
}
