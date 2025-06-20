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

const (
	// envLogLevel is the environment variable that can be used to override the log level.
	envLogLevel     = "RADIANCE_LOG_LEVEL"
	defaultLogLevel = "info"
)

var (
	initMutex   sync.Mutex
	initialized bool
)

// Init initializes the common components of the application. This includes setting up the directories
// for data and logs, initializing the logger, and setting up reporting.
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

	err = initLogger(filepath.Join(logDir, app.LogFileName), logLevel)
	if err != nil {
		return fmt.Errorf("initialize log: %w", err)
	}
	initialized = true
	return nil
}

// initLogger reconfigures the default slog.Logger to write to a file and stdout and sets the log level.
// The log level is determined, first by the environment variable if set and valid, then by the provided level.
// If both are invalid and/or not set, it defaults to "info".
func initLogger(logPath, level string) error {
	var lvl slog.Level
	var err error
	envLvl := os.Getenv(envLogLevel)
	if envLvl != "" {
		if lvl, err = internal.ParseLogLevel(envLvl); err != nil {
			slog.Warn("Failed to parse "+envLogLevel, "error", err)
		} else {
			envLvl = ""
		}
	}
	if envLvl == "" && level != "" {
		if lvl, err = internal.ParseLogLevel(level); err != nil {
			slog.Warn("Failed to parse given log level", "error", err)
		}
	}
	slog.SetLogLoggerLevel(lvl)

	// If the log file does not exist, create it.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	// defer f.Close() - file should be closed externally when logger is no longer needed
	logWriter := io.MultiWriter(os.Stdout, f)
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		AddSource: true,
		Level:     lvl,
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
