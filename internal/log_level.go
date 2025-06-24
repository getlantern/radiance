package internal

import (
	"fmt"
	"log/slog"
	"strings"
)

const (
	// slog does not define trace and fatal levels, so we define them here.
	LevelTrace = slog.LevelDebug - 4
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
	LevelFatal = slog.LevelError + 4
	LevelPanic = slog.LevelError + 8
)

// ParseLogLevel parses a string representation of a log level and returns the corresponding slog.Level.
// If the level is not recognized, it returns LevelInfo.
func ParseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	case "fatal":
		return LevelFatal, nil
	case "panic":
		return LevelPanic, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level: %s", level)
	}
}

func FormatLogLevel(level slog.Level) string {
	switch {
	case level < LevelDebug:
		return "TRACE"
	case level < LevelInfo:
		return "DEBUG"
	case level < LevelWarn:
		return "INFO"
	case level < LevelError:
		return "WARN"
	case level < LevelFatal:
		return "ERROR"
	case level < LevelPanic:
		return "FATAL"
	default:
		return "PANIC"
	}
}
