package radiance

import (
	"context"
	"fmt"
	"log/slog"
)

// TODO: to be removed ater a custom implementation of logger has been made
type Logger interface {
	Trace(args ...any)
	Debug(args ...any)
	Info(args ...any)
	Warn(args ...any)
	Error(args ...any)
	Fatal(args ...any)
	Panic(args ...any)
}

// TODO: to be removed ater a custom implementation of context logger has been made
type ContextLogger interface {
	Logger
	TraceContext(ctx context.Context, args ...any)
	DebugContext(ctx context.Context, args ...any)
	InfoContext(ctx context.Context, args ...any)
	WarnContext(ctx context.Context, args ...any)
	ErrorContext(ctx context.Context, args ...any)
	FatalContext(ctx context.Context, args ...any)
	PanicContext(ctx context.Context, args ...any)
}

func Info(args ...any) {
	if len(args) == 0 {
		slog.Info("")
		return
	}
	msg := stringify(args[0])
	slog.Info(msg, args[1:]...)
}

func stringify(input any) string {
	if str, ok := input.(string); ok {
		return str
	}
	if stringer, ok := input.(fmt.Stringer); ok {
		return stringer.String()
	}
	return fmt.Sprintf("%v", input)
}
