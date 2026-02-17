package internal

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
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

	Disable = slog.LevelInfo + 1000 // A level that disables logging, used for testing or no-op logger.
)

func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format("2006-01-02 15:04:05.000 UTC"))
				}
				return a
			case slog.SourceKey:
				source, ok := a.Value.Any().(*slog.Source)
				if !ok {
					return a
				}
				// remove github.com/<username> to get pkg name
				var service, fn string
				fields := strings.SplitN(source.Function, "/", 4)
				switch len(fields) {
				case 0, 1, 2:
					file := filepath.Base(source.File)
					a.Value = slog.StringValue(fmt.Sprintf("%s:%d", file, source.Line))
					return a
				case 3:
					pf := strings.SplitN(fields[2], ".", 2)
					service, fn = pf[0], pf[1]
				default:
					service = fields[2]
					fn = strings.SplitN(fields[3], ".", 2)[1]
				}

				_, file, fnd := strings.Cut(source.File, service+"/")
				if !fnd {
					file = filepath.Base(source.File)
				}
				src := slog.GroupValue(
					slog.String("func", fn),
					slog.String("file", fmt.Sprintf("%s:%d", file, source.Line)),
				)
				a.Value = slog.GroupValue(
					slog.String("service", service),
					slog.Any("source", src),
				)
				a.Key = ""
			case slog.LevelKey:
				// format the log level to account for the custom levels defined in internal/util.go, i.e. trace
				// otherwise, slog will print as "DEBUG-4" (trace) or similar
				level := a.Value.Any().(slog.Level)
				a.Value = slog.StringValue(FormatLogLevel(level))
			}
			return a
		},
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

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
	case "disable", "none", "off":
		return Disable, nil
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

// NoOpLogger returns a no-op logger that does not log anything.
func NoOpLogger() *slog.Logger {
	// Create a no-op logger that does nothing.
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: Disable,
	}))
}
