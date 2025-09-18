// Package common contains common initialization code and utilities for the Radiance application.
package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/appdir"
	"github.com/getlantern/osversion"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/metrics"
	"github.com/getlantern/radiance/vpn/ipc"
)

const (
	defaultLogLevel = "info"
)

var (
	initMutex   sync.Mutex
	initialized bool

	dataPath                    atomic.Value
	logPath                     atomic.Value
	oldConfig                   *config
	configFileWatcher           *internal.FileWatcher
	shutdownOTEL                func(context.Context) error
	harvestConnections          sync.Once
	harvestConnectionTickerStop func()
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

	// creating file watcher to monitor changes to config file and initialize whatever depends on it
	configPath := filepath.Join(dataDir, ConfigFileName)
	slog.Debug("created config file watcher for opentelemetry")
	configFileWatcher = internal.NewFileWatcher(configPath, func() {
		afterConfigIsAvailableCallback(configPath, deviceID)
	})
	configFileWatcher.Start()
	initialized = true
	return nil
}

func afterConfigIsAvailableCallback(configPath, deviceID string) {
	f, err := os.Open(configPath)
	if err != nil {
		slog.Error("Failed to open config file", "error", err)
		return
	}
	defer f.Close()
	// unmarshaling with json is fine here since we only care about OTEL section of the config
	cfg := new(config)
	if err := json.NewDecoder(f).Decode(cfg); err != nil {
		slog.Error("Failed to decode config file", "error", err)
		return
	}

	// Check if the old OTEL configuration is the same as the new one.
	if oldConfig != nil && reflect.DeepEqual(oldConfig.ConfigResponse.OTEL, cfg.ConfigResponse.OTEL) {
		slog.Debug("OpenTelemetry configuration has not changed, skipping initialization")
		return
	}

	if err := initOTEL(deviceID, cfg.ConfigResponse); err != nil {
		slog.Error("Failed to initialize OpenTelemetry", "error", err)
		return
	}

	harvestConnections.Do(func() {
		harvestConnectionTickerStop = metrics.HarvestConnectionMetrics(1 * time.Minute)
	})
}

func Close(ctx context.Context) error {
	initMutex.Lock()
	defer initMutex.Unlock()
	if !initialized {
		return nil
	}

	var errs error
	if configFileWatcher != nil {
		if err := configFileWatcher.Close(); err != nil {
			errs = errors.Join(errs, fmt.Errorf("failed to close config file watcher: %w", err))
		}
	}

	// stop collecting connection metrics
	if harvestConnectionTickerStop != nil {
		harvestConnectionTickerStop()
	}

	if shutdownOTEL != nil {
		slog.Info("Shutting down existing OpenTelemetry SDK")
		if err := shutdownOTEL(ctx); err != nil {
			slog.Error("Failed to shutdown OpenTelemetry SDK", "error", err)
			errs = errors.Join(errs, fmt.Errorf("failed to shutdown OpenTelemetry SDK: %w", err))
		}
		shutdownOTEL = nil
	}

	return errs
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

type config struct {
	ConfigResponse C.ConfigResponse
}

func initOTEL(deviceID string, configResponse C.ConfigResponse) error {
	initMutex.Lock()
	defer initMutex.Unlock()

	if configResponse.OTEL.Endpoint == "" {
		slog.Debug("OpenTelemetry configuration has not changed, skipping initialization")
		return nil
	}

	if shutdownOTEL != nil {
		slog.Info("Shutting down existing OpenTelemetry SDK")
		if err := shutdownOTEL(context.Background()); err != nil {
			slog.Error("Failed to shutdown OpenTelemetry SDK", "error", err)
			return fmt.Errorf("failed to shutdown OpenTelemetry SDK: %w", err)
		}
		shutdownOTEL = nil
	}

	attrs := metrics.Attributes{
		App:        "radiance",
		DeviceID:   deviceID,
		AppVersion: ClientVersion,
		Platform:   Platform,
		GoVersion:  runtime.Version(),
		OSName:     runtime.GOOS,
		OSArch:     runtime.GOARCH,
		GeoCountry: configResponse.Country,
		Pro:        configResponse.Pro,
	}
	if osStr, err := osversion.GetHumanReadable(); err == nil {
		attrs.OSVersion = osStr
	}

	shutdown, err := metrics.SetupOTelSDK(context.Background(), attrs, configResponse)
	if err != nil {
		slog.Error("Failed to start OpenTelemetry SDK", "error", err)
		return fmt.Errorf("failed to start OpenTelemetry SDK: %w", err)
	}

	shutdownOTEL = shutdown
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
