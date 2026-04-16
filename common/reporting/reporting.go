package reporting

import (
	"context"
	"log/slog"
	"runtime/debug"

	"github.com/getlantern/radiance/internal"
)

// PanicListener logs a panic message. It is passed as a callback to
// libraries that need a panic notification hook.
func PanicListener(msg string) {
	slog.Log(context.Background(), internal.LevelPanic, msg,
		"stack", string(debug.Stack()),
	)
}
