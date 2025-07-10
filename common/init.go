package common

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/getlantern/appdir"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/internal"
)

const (
	defaultLogLevel = "info"
)

var (
	initMutex   sync.Mutex
	initialized bool

	dataPath atomic.Value
	logPath  atomic.Value
)

func init() {
	// ensure dataPath and logPath are of type string
	dataPath.Store("")
	logPath.Store("")

}

// Init initializes the common components of the application. This includes setting up the directories
// for data and logs, initializing the logger, and setting up reporting.
func Init(dataDir, logDir, logLevel string) error {
	initMutex.Lock()
	defer initMutex.Unlock()
	if initialized {
		return nil
	}

	reporting.Init(Version)
	err := SetupDirectories(dataDir, logDir)
	if err != nil {
		return fmt.Errorf("failed to setup directories: %w", err)
	}

	err = initLogger(filepath.Join(logDir, LogFileName), logLevel)
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
	if elevel, hasLevel := env.Get[string](env.LogLevel); hasLevel {
		level = elevel
	}
	var lvl slog.Level
	if level != "" {
		var err error
		lvl, err = internal.ParseLogLevel(level)
		if err != nil {
			slog.Warn("Failed to parse log level", "error", err)
		} else {
			slog.SetLogLoggerLevel(lvl)
		}
	}

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

func SetupDirectories(data, logs string) error {
	if d, ok := env.Get[string](env.DataPath); ok {
		data = d
	} else if data == "" {
		data = appdir.General(Name)
		data = maybeAddSuffix(data, "data")
	}
	if l, ok := env.Get[string](env.LogPath); ok {
		logs = l
	} else if logs == "" {
		logs = appdir.Logs(Name)
		logs = maybeAddSuffix(logs, "logs")
	}
	for _, path := range []string{data, logs} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", path, err)
		}
	}

	dataPath.Store(data)
	logPath.Store(logs)
	return nil
}

func maybeAddSuffix(path, suffix string) string {
	if filepath.Base(path) != suffix {
		path = filepath.Join(path, suffix)
	}
	return path
}

func DataPath() string {
	return dataPath.Load().(string)
}

func LogPath() string {
	return logPath.Load().(string)
}
