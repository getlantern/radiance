// Package common contains common initialization code and utilities for the Radiance application.
package common

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
)

var (
	initialized atomic.Bool
)

func Env() string {
	return strings.ToLower(env.GetString(env.ENV))
}

// Prod returns true if the application is running in production environment.
// Treating ENV == "" as production is intentional: if RADIANCE_ENV is unset,
// we default to production mode to ensure the application runs with safe, non-debug settings.
func Prod() bool {
	e := Env()
	return e == "production" || e == "prod" || e == ""
}

// Dev returns true if the application is running in development environment.
func Dev() bool {
	e := Env()
	return e == "development" || e == "dev"
}

// Stage returns true if the application is running in staging environment.
func Stage() bool {
	e := Env()
	return e == "stage" || e == "staging"
}

func init() {
	if env.GetBool(env.Testing) {
		slog.SetDefault(log.NoOpLogger())
		slog.SetLogLoggerLevel(log.Disable)
	}
}

// Init initializes the common components of the application. This includes setting up the directories
// for data and logs, initializing the logger, and setting up reporting.
func Init(dataDir, logDir, logLevel string) (err error) {
	slog.Info("Initializing common package")
	if initialized.Swap(true) {
		return nil
	}
	defer func() {
		if err != nil {
			initialized.Store(false)
		}
	}()

	reporting.Init(Version)
	data, logs, err := setupDirectories(dataDir, logDir)
	if err != nil {
		return fmt.Errorf("failed to setup directories: %w", err)
	}

	if err = settings.InitSettings(data); err != nil {
		return fmt.Errorf("failed to initialize settings: %w", err)
	}

	settings.Set(settings.DataPathKey, data)
	settings.Set(settings.LogPathKey, logs)
	settings.Set(settings.LogLevelKey, logLevel)

	logger := log.NewLogger(log.Config{
		LogPath: filepath.Join(logs, internal.LogFileName),
		Level:   logLevel,
		Prod:    Prod(),
	})
	slog.SetDefault(logger)

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
	crashFilePath := filepath.Join(settings.GetString(settings.LogPathKey), internal.CrashLogFileName)
	f, err := os.OpenFile(crashFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		slog.Error("Failed to open crash log file", "error", err)
	} else {
		debug.SetCrashOutput(f, debug.CrashOptions{})
		// We can close f after SetCrashOutput because it duplicates the file descriptor.
		f.Close()
	}
}

// setupDirectories creates the data and logs directories, and needed subdirectories if they do
// not exist. If data or logs are the empty string, it will use the user's config directory retrieved
// from the OS.
func setupDirectories(data, logs string) (dataDir, logDir string, err error) {
	if path := env.GetString(env.DataPath); path != "" {
		data = path
	} else if data == "" {
		data = internal.DefaultDataPath()
	}
	if path := env.GetString(env.LogPath); path != "" {
		logs = path
	} else if logs == "" {
		logs = internal.DefaultLogPath()
	}
	// ensure the data and logs directories end with the correct suffix
	data = maybeAddSuffix(data, "data")
	logs = maybeAddSuffix(logs, "logs")
	data, _ = filepath.Abs(data)
	logs, _ = filepath.Abs(logs)
	for _, path := range []string{data, logs} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return data, logs, fmt.Errorf("failed to create directory %s: %w", path, err)
		}
	}
	return data, logs, nil
}

func maybeAddSuffix(path, suffix string) string {
	if !strings.EqualFold(filepath.Base(path), suffix) {
		path = filepath.Join(path, suffix)
	}
	return path
}
