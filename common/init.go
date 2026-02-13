// Package common contains common initialization code and utilities for the Radiance application.
package common

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/getlantern/appdir"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
)

var (
	initialized atomic.Bool
)

func Env() string {
	e, _ := env.Get[string](env.ENV)
	e = strings.ToLower(e)
	return e
}

// Prod returns true if the application is running in production environment.
// Treating ENV == "" as production is intentional: if RADIANCE_ENV is unset,
// we default to production mode to ensure the application runs with safe, non-debug settings.
func Prod() bool {
	e, _ := env.Get[string](env.ENV)
	e = strings.ToLower(e)
	return e == "production" || e == "prod" || e == ""
}

// Dev returns true if the application is running in development environment.
func Dev() bool {
	e, _ := env.Get[string](env.ENV)
	e = strings.ToLower(e)
	return e == "development" || e == "dev"
}

// Stage returns true if the application is running in staging environment.
func Stage() bool {
	e, _ := env.Get[string](env.ENV)
	return e == "stage" || e == "staging"
}

// Init initializes the common components of the application. This includes setting up the directories
// for data and logs, initializing the logger, and setting up reporting.
func Init(dataDir, logDir, logLevel string) error {
	slog.Info("Initializing common package")
	return initialize(dataDir, logDir, logLevel, false)
}

// InitReadOnly locates the settings file in provided directory and initializes the common components
// in read-only mode using the necessary settings from the settings file. This is used in contexts
// where settings should not be modified, such as in the IPC server or other auxiliary processes.
func InitReadOnly(dataDir, logDir, logLevel string) error {
	slog.Info("Initializing in read-only")
	return initialize(dataDir, logDir, logLevel, true)
}

func initialize(dataDir, logDir, logLevel string, readonly bool) error {
	if initialized.Swap(true) {
		return nil
	}

	reporting.Init(Version)
	data, logs, err := setupDirectories(dataDir, logDir)
	if err != nil {
		return fmt.Errorf("failed to setup directories: %w", err)
	}
	if readonly {
		// in read-only mode, favor settings from the settings file if given parameters are empty
		if logDir == "" && settings.GetString(settings.LogPathKey) != "" {
			logs = settings.GetString(settings.LogPathKey)
		}
		if settings.GetString(settings.LogLevelKey) != "" {
			logLevel = settings.GetString(settings.LogLevelKey)
		}
	}
	err = initLogger(filepath.Join(logs, LogFileName), logLevel)
	if err != nil {
		slog.Error("Error initializing logger", "error", err)
		return fmt.Errorf("initialize log: %w", err)
	}

	if readonly {
		settings.SetReadOnly(true)
		if err := settings.StartWatching(); err != nil {
			return fmt.Errorf("start watching settings file: %w", err)
		}
	} else {
		settings.Set(settings.DataPathKey, data)
		settings.Set(settings.LogPathKey, logs)
		settings.Set(settings.LogLevelKey, logLevel)
	}

	slog.Info("Using data and log directories", "dataDir", data, "logDir", logs)
	createCrashReporter()
	if Dev() {
		logModuleInfo()
	}
	return nil
}

func logModuleInfo() {
	if bi, ok := debug.ReadBuildInfo(); ok {
		slog.Debug("Build Information:", "goversion", bi.GoVersion, "main module", bi.Main.Path+" @ "+bi.Main.Version)
		if len(bi.Deps) > 0 {
			slog.Debug("Dependencies:")
			for _, dep := range bi.Deps {
				slog.Debug("dep", "path", dep.Path, "version", dep.Version)
			}
		} else {
			slog.Debug("No dependencies found.\n")
		}
	} else {
		slog.Info("No build information available.")
	}
}

func createCrashReporter() {
	crashFilePath := filepath.Join(settings.GetString(settings.LogPathKey), "lantern_crash.log")
	f, err := os.OpenFile(crashFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		slog.Error("Failed to open crash log file", "error", err)
	} else {
		debug.SetCrashOutput(f, debug.CrashOptions{})
		// We can close f after SetCrashOutput because it duplicates the file descriptor.
		f.Close()
	}
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

	// lumberjack will create the log file if it does not exist with permissions 0600 otherwise it
	// carries over the existing permissions. So we create it here with 0644 so we don't need root/admin
	// privileges or chown/chmod to read it.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		slog.Warn("Failed to pre-create log file", "error", err, "path", logPath)
	} else {
		f.Close()
	}

	logRotator := &lumberjack.Logger{
		Filename:   logPath, // Log file path
		MaxSize:    25,      // Rotate log when it reaches 25 MB
		MaxBackups: 2,       // Keep up to 2 rotated log files
		MaxAge:     30,      // Retain old log files for up to 30 days
		Compress:   Prod(),  // Compress rotated log files
	}

	loggingToStdOut := true
	var logWriter io.Writer
	if noStdout, _ := env.Get[bool](env.DisableStdout); noStdout {
		logWriter = logRotator
		loggingToStdOut = false
	} else if isWindowsProd() {
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
	runtime.AddCleanup(&logWriter, func(f *os.File) {
		f.Close()
	}, f)
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		AddSource: true,
		Level:     lvl,
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
				a.Value = slog.StringValue(internal.FormatLogLevel(level))
			}
			return a
		},
	}))
	if !loggingToStdOut {
		if IsWindows() {
			fmt.Printf("Logging to file only on Windows prod -- run with RADIANCE_ENV=dev to enable stdout path: %s, level: %s\n", logPath, internal.FormatLogLevel(lvl))
		} else {
			fmt.Printf("Logging to file only -- RADIANCE_DISABLE_STDOUT_LOG is set path: %s, level: %s\n", logPath, internal.FormatLogLevel(lvl))
		}
	} else {
		fmt.Printf("Logging to file and stdout path: %s, level: %s\n", logPath, internal.FormatLogLevel(lvl))
	}
	slog.SetDefault(logger)
	return nil
}

func isWindowsProd() bool {
	if !IsWindows() {
		return false
	}
	return !Dev()
}

// setupDirectories creates the data and logs directories, and needed subdirectories if they do
// not exist. If data or logs are the empty string, it will use the user's config directory retrieved
// from the OS.
func setupDirectories(data, logs string) (dataDir, logDir string, err error) {
	if d, ok := env.Get[string](env.DataPath); ok {
		data = d
	} else if data == "" {
		data = outDir("data")
	}
	if l, ok := env.Get[string](env.LogPath); ok {
		logs = l
	} else if logs == "" {
		logs = outDir("logs")
	}
	data, _ = filepath.Abs(data)
	logs, _ = filepath.Abs(logs)
	for _, path := range []string{data, logs} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return data, logs, fmt.Errorf("failed to create directory %s: %w", path, err)
		}
	}
	if err := settings.InitSettings(data); err != nil {
		return data, logs, fmt.Errorf("failed to initialize settings: %w", err)
	}
	return data, logs, nil
}

func outDir(subdir string) string {
	var data string
	var name string
	if IsWindows() || IsMacOS() {
		name = capitalizeFirstLetter(Name)
	} else {
		name = Name
	}
	if IsWindows() {
		publicDir := os.Getenv("Public")
		data = filepath.Join(publicDir, name)
	} else {
		data = appdir.General(name)
	}
	return maybeAddSuffix(data, subdir)
}

func capitalizeFirstLetter(s string) string {
	if s == "" {
		return ""
	}

	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError { // Handle invalid UTF-8 sequences
		return s // Or handle error as needed
	}

	return string(unicode.ToUpper(r)) + s[size:]
}

func maybeAddSuffix(path, suffix string) string {
	if filepath.Base(path) != suffix {
		path = filepath.Join(path, suffix)
	}
	return path
}
