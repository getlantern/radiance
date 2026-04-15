package reporting

import (
	"context"
	"log/slog"
)

const levelPanic = slog.LevelError + 8

// PanicListener logs a panic message. It is passed as a callback to
// libraries that need a panic notification hook.
func PanicListener(msg string) {
	slog.Log(context.Background(), levelPanic, msg)
}
