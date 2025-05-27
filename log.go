package radiance

import (
	"context"
	"fmt"
	"log/slog"
)

// TODO: to be removed ater a custom implementation of logger has been made
//
// As provided by signbox:
//
// type Logger interface {
// 	Trace(args ...any)
// 	Debug(args ...any)
// 	Info(args ...any)
// 	Warn(args ...any)
// 	Error(args ...any)
// 	Fatal(args ...any)
// 	Panic(args ...any)
// }
//
// type ContextLogger interface {
// 	Logger
// 	TraceContext(ctx context.Context, args ...any)
// 	DebugContext(ctx context.Context, args ...any)
// 	InfoContext(ctx context.Context, args ...any)
// 	WarnContext(ctx context.Context, args ...any)
// 	ErrorContext(ctx context.Context, args ...any)
// 	FatalContext(ctx context.Context, args ...any)
// 	PanicContext(ctx context.Context, args ...any)
// }

type radLogger struct {
	logger *slog.Logger
}
type radCtxLogger struct {
	logger *slog.Logger
}

func NewRadLogger(logger *slog.Logger) *radLogger {
	return &radLogger{logger: logger}
}

func NewRadCtxLogger(logger *slog.Logger) *radCtxLogger {
	return &radCtxLogger{logger: logger}
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

func (l *radLogger) Trace(args ...any) {
	ctx := context.Background()
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelDebug, msg, args[1:]...) // Use LevelDebug for Trace
}

func (l *radLogger) Debug(args ...any) {
	ctx := context.Background()
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelDebug, msg, args[1:]...)
}

func (l *radLogger) Info(args ...any) {
	ctx := context.Background()
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelInfo, msg, args[1:]...)
}

func (l *radLogger) Warn(args ...any) {
	ctx := context.Background()
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelWarn, msg, args[1:]...)
}

func (l *radLogger) Error(args ...any) {
	ctx := context.Background()
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelError, msg, args[1:]...)
}

func (l *radLogger) Fatal(args ...any) {
	ctx := context.Background()
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelError, msg, args[1:]...)
	panic(msg) // Fatal typically terminates the program
}

func Panic(args ...any) {
	ctx := context.Background()
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelError, msg, args[1:]...)
	panic(msg)
}

func (l *radCtxLogger) TraceContext(ctx context.Context, args ...any) {
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelDebug, msg, args[1:]...)
}

func (l *radCtxLogger) DebugContext(ctx context.Context, args ...any) {
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelDebug, msg, args[1:]...)
}

func (l *radCtxLogger) InfoContext(ctx context.Context, args ...any) {
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelInfo, msg, args[1:]...)
}

func (l *radCtxLogger) WarnContext(ctx context.Context, args ...any) {
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelWarn, msg, args[1:]...)
}

func (l *radCtxLogger) ErrorContext(ctx context.Context, args ...any) {
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelError, msg, args[1:]...)
}

func (l *radCtxLogger) FatalContext(ctx context.Context, args ...any) {
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelError, msg, args[1:]...)
	panic(msg)
}

func (l *radCtxLogger) PanicContext(ctx context.Context, args ...any) {
	var msg string
	if len(args) > 0 {
		msg = stringify(args[0])
	}
	slog.Log(ctx, slog.LevelError, msg, args[1:]...)
	panic(msg)
}
