package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/fileperm"
	"github.com/getlantern/radiance/common/settings"

	lsync "github.com/getlantern/common/sync"
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

type Config struct {
	// LogPath is the full path to the log file.
	LogPath string
	// Level is the log level string (e.g., "info", "debug").
	Level string
	// Prod indicates whether the application is running in production mode.
	Prod bool
	// DisablePublisher indicates whether to disable the log publisher which is used for real-time
	// log streaming.
	DisablePublisher bool
}

// NewLogger creates and returns a configured *slog.Logger that writes to a rotating log file
// and optionally to stdout.
// Returns noop logger if log level is set to disable.
func NewLogger(cfg Config) *slog.Logger {
	if env.GetBool(env.Testing) {
		return NoOpLogger()
	}
	level := settings.GetString(settings.LogLevelKey)
	if level == "" {
		level = env.GetString(env.LogLevel)
	}
	if level == "" && cfg.Level != "" {
		level = cfg.Level
	}
	slevel, err := ParseLogLevel(level)
	if err != nil {
		slog.Warn("Failed to parse log level", "error", err)
	}
	slog.SetLogLoggerLevel(slevel)
	leveler := settingsLeveler{fallback: slevel}

	// lumberjack creates the log file with 0600 if it does not exist, otherwise it carries over
	// the existing permissions. Pre-create with [fileperm.File] so the platform-appropriate mode is
	// applied.
	f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileperm.File)
	if err != nil {
		slog.Warn("Failed to pre-create log file", "error", err, "path", cfg.LogPath)
	} else {
		f.Close()
	}

	logRotator := &lumberjack.Logger{
		Filename:   cfg.LogPath, // Log file path
		MaxSize:    25,          // Rotate log when it reaches 25 MB
		MaxBackups: 2,           // Keep up to 2 rotated log files
		MaxAge:     30,          // Retain old log files for up to 30 days
		Compress:   cfg.Prod,    // Compress rotated log files
	}

	isWindows := runtime.GOOS == "windows"
	isWindowsProd := isWindows && cfg.Prod

	loggingToStdOut := true
	var logWriter io.Writer
	if env.GetBool(env.DisableStdout) {
		logWriter = logRotator
		loggingToStdOut = false
	} else if isWindowsProd {
		// For some reason, logging to both stdout and a file on Windows
		// causes issues with some Windows services where the logs
		// do not get written to the file. So in prod mode on Windows,
		// we log to file only. See:
		// https://www.reddit.com/r/golang/comments/1fpo3cg/golang_windows_service_cannot_write_log_files/
		logWriter = logRotator
		loggingToStdOut = false
	} else {
		logWriter = io.MultiWriter(os.Stdout, logRotator)
	}
	if !cfg.DisablePublisher {
		logWriter = io.MultiWriter(logWriter, Publisher())
	}
	var handler slog.Handler = slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		Level: leveler,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format("2006-01-02 15:04:05.000 UTC"))
				}
			case slog.LevelKey:
				// format the log level to account for the custom levels, i.e. trace
				// otherwise, slog will print as "DEBUG-4" (trace) or similar
				level, ok := a.Value.Any().(slog.Level)
				if ok {
					a.Value = slog.StringValue(FormatLogLevel(level))
					break
				}
				vtype := fmt.Sprintf("%T", a.Value.Any())
				return slog.Group("",
					slog.String("level", FormatLogLevel(slog.LevelWarn)),
					slog.String("error", "unexpected type for level: "+vtype),
					slog.Any("level-"+vtype, a.Value),
				)
			}
			return a
		},
	})
	handler = newSourceHandler(handler)
	handler = &Handler{Handler: handler, w: logWriter}
	logger := slog.New(handler)
	if !loggingToStdOut {
		if isWindows {
			fmt.Printf("Logging to file only on Windows prod -- run with RADIANCE_ENV=dev to enable stdout path: %s, level: %s\n", cfg.LogPath, FormatLogLevel(slevel))
		} else {
			fmt.Printf("Logging to file only -- RADIANCE_DISABLE_STDOUT_LOG is set path: %s, level: %s\n", cfg.LogPath, FormatLogLevel(slevel))
		}
	} else {
		fmt.Printf("Logging to file and stdout path: %s, level: %s\n", cfg.LogPath, FormatLogLevel(slevel))
	}
	return logger
}

type Handler struct {
	slog.Handler
	w io.Writer
}

func (h *Handler) Writer() io.Writer {
	return h.w
}

// sourceHandler adds a cached, human-readable source attribute derived from the
// record PC. This keeps source formatting out of ReplaceAttr and avoids
// recomputing it for repeated call sites.
type sourceHandler struct {
	slog.Handler
	cache *lsync.TypedMap[uintptr, slog.Attr]
}

func newSourceHandler(next slog.Handler) slog.Handler {
	return &sourceHandler{
		Handler: next,
		cache:   new(lsync.TypedMap[uintptr, slog.Attr]),
	}
}

func (h *sourceHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.PC == 0 {
		return h.Handler.Handle(ctx, r)
	}

	r = r.Clone()
	r.AddAttrs(h.sourceAttr(r.PC))
	return h.Handler.Handle(ctx, r)
}

func (h *sourceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.withHandler(h.Handler.WithAttrs(attrs))
}

func (h *sourceHandler) WithGroup(name string) slog.Handler {
	return h.withHandler(h.Handler.WithGroup(name))
}

func (h *sourceHandler) withHandler(next slog.Handler) slog.Handler {
	return &sourceHandler{
		Handler: next,
		cache:   h.cache,
	}
}

func (h *sourceHandler) sourceAttr(pc uintptr) slog.Attr {
	if attr, ok := h.cache.Load(pc); ok {
		return attr
	}

	pcs := [1]uintptr{pc}
	frame, _ := runtime.CallersFrames(pcs[:]).Next()

	attr := formatSource(frame.Function, frame.File, frame.Line)
	h.cache.Store(pc, attr)
	return attr
}

func formatSource(function, file string, line int) slog.Attr {
	pkg, fn, ok := parseSourceFunction(function)
	if !ok {
		return slog.String(slog.SourceKey, fmt.Sprintf("%s:%d", filepath.Base(file), line))
	}

	return slog.Group("",
		slog.String("pkg", pkg),
		slog.Group("source",
			slog.String("func", fn),
			slog.String("file", fmt.Sprintf("%s:%d", trimSourceFile(file, pkg), line)),
		),
	)
}

// parseSourceFunction splits a runtime function symbol into a package label and
// a function name. The label is the symbol's third slash-separated segment, so
// for a github.com/org/repo/leaf path it is the repo name, not the leaf package:
//
//   - "github.com/org/repo/leaf.Func"           → "repo", "Func"
//   - "github.com/org/repo/leaf.(*Type).Method" → "repo", "(*Type).Method"
//
// ok is false when the symbol has fewer than three segments.
func parseSourceFunction(symbol string) (pkg string, fn string, ok bool) {
	pathParts := strings.SplitN(symbol, "/", 4)

	switch len(pathParts) {
	case 3:
		pkg, fn, ok = strings.Cut(pathParts[2], ".")
		return pkg, fn, ok && pkg != "" && fn != ""
	case 4:
		pkg = pathParts[2]
		if pkg == "" {
			return "", "", false
		}

		_, fn, ok = strings.Cut(pathParts[3], ".")
		return pkg, fn, ok && fn != ""
	default:
		return "", "", false
	}
}

func trimSourceFile(file, pkg string) string {
	if _, after, found := strings.Cut(file, pkg+"/"); found && after != "" {
		return after
	}
	return filepath.Base(file)
}

// settingsLeveler reads the current log level from settings on each call,
// so changes to settings.LogLevelKey take effect without rebuilding the logger.
type settingsLeveler struct {
	fallback slog.Level
}

func (s settingsLeveler) Level() slog.Level {
	if v := settings.GetString(settings.LogLevelKey); v != "" {
		if lvl, err := ParseLogLevel(v); err == nil {
			return lvl
		}
	}
	return s.fallback
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
