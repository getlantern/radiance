// Package common contains common initialization code and utilities for the Radiance application.
package common

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/appdir"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/vpn/ipc"
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

	if v, _ := env.Get[bool](env.Testing); v {
		slog.SetLogLoggerLevel(internal.Disable)
	}
}

// Init initializes the common components of the application. This includes setting up the directories
// for data and logs, initializing the logger, and setting up reporting.
func Init(dataDir, logDir, logLevel, deviceID string) error {
	initMutex.Lock()
	defer initMutex.Unlock()
	if initialized {
		return nil
	}

	reporting.Init(Version)
	err := setupDirectories(dataDir, logDir)
	if err != nil {
		return fmt.Errorf("failed to setup directories: %w", err)
	}

	dataDir = dataPath.Load().(string)
	logDir = logPath.Load().(string)

	err = initLogger(filepath.Join(logDir, LogFileName), logLevel)
	if err != nil {
		return fmt.Errorf("initialize log: %w", err)
	}

	if runtime.GOOS != "windows" {
		ipc.SetSocketPath(dataDir)
	}

	initialized = true
	return nil
}

func Close(ctx context.Context) error {
	initMutex.Lock()
	defer initMutex.Unlock()
	if !initialized {
		return nil
	}

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
	if lvl == internal.Disable {
		return nil
	}

	// If the log file does not exist, create it.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	var logWriter io.Writer
	if noStdout, _ := env.Get[bool](env.DisableStdout); noStdout {
		logWriter = f
	} else {
		logWriter = io.MultiWriter(os.Stdout, f)
	}
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		AddSource: true,
		Level:     lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format("2006-01-02 15:04:05.000"))
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
				a.Value = slog.StringValue(internal.FormatLogLevel(level))
			}
			return a
		},
	}))
	slog.SetDefault(logger)
	return nil
}

// setupDirectories creates the data and logs directories, and needed subdirectories if they do
// not exist. If data or logs are the empty string, it will use the user's config directory retrieved
// from the OS. The resulting paths are stored in [dataPath] and [logPath] respectively.
func setupDirectories(data, logs string) error {
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
	data, _ = filepath.Abs(data)
	logs, _ = filepath.Abs(logs)
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
