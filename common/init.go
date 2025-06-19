package common

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/internal"
)

var (
	initMutex   sync.Mutex
	initialized bool
)

func Init(dataDir, logDir, logLevel string) error {
	initMutex.Lock()
	defer initMutex.Unlock()
	if initialized {
		return nil
	}

	reporting.Init(Version)
	dataDir, logDir, err := SetupDirectories(dataDir, logDir)
	if err != nil {
		return fmt.Errorf("failed to setup directories: %w", err)
	}

	var level slog.Level
	if lvl := os.Getenv("RADIANCE_LOG_LEVEL"); lvl != "" {
		if parsedLevel, err := internal.ParseLogLevel(lvl); err == nil {
			level = parsedLevel
		} else {
			slog.Warn("Failed to parse RADIANCE_LOG_LEVEL, using default level", "error", err)
		}
	}
	err = initLogger(filepath.Join(logDir, app.LogFileName), level)
	if err != nil {
		return fmt.Errorf("initialize log: %w", err)
	}
	initialized = true
	return nil
}

// initLogger reconfigures the default [slog.Logger] to log to both stdout and a file.
func initLogger(logPath string, level slog.Level) error {
	// If the log file does not exist, create it.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	// defer f.Close() - file should be closed externally when logger is no longer needed
	logWriter := io.MultiWriter(os.Stdout, f)
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.SourceKey:
				source := a.Value.Any().(*slog.Source)
				source.File = filepath.Base(source.File) // Shorten the file path to just the file name
			case slog.LevelKey:
				// format the log level to account for the custom levels defined in internal/util.go, i.e. trace
				// otherwise, slog will print as "DEBUG-4" (trace) or similar
				level := a.Value.Any().(slog.Level)
				a.Value = slog.StringValue(internal.FormatLogLevel(level))
			}
			return a
		},
	}))
	slog.SetDefault(logger)
	return nil
}
