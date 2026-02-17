package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "unsafe" // for go:linkname

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/traces"
	"github.com/getlantern/radiance/vpn"
	"github.com/getlantern/radiance/vpn/ipc"
)

const serviceName = "lanternd"

func main() {
	run()
}

func runDaemon(dataPath, logPath, logLevel string) error {
	dataPath = os.ExpandEnv(dataPath)
	logPath = os.ExpandEnv(logPath)

	slog.Info("Starting lanternd", "version", common.Version, "dataPath", dataPath)

	if err := settings.InitSettings(dataPath); err != nil {
		return fmt.Errorf("failed to initialize settings: %w", err)
	}
	// temporarily set settings to read-only to prevent changes until we reload if needed.
	settings.SetReadOnly(true)

	if err := common.Init(dataPath, logPath, logLevel); err != nil {
		return fmt.Errorf("failed to initialize common components: %w", err)
	}

	// we need to reload settings if the data path was changed via IPC. we want to keep the original
	// settings file so we know if/where to reload from next time.
	// This is temporary and will be removed once we move ownership and interaction of all files to
	// one process.
	settingsPath := filepath.Dir(settings.GetString("file_path"))
	path := settings.GetString(settings.DataPathKey)
	if path != "" && path != settingsPath {
		slog.Info("Reloading settings", "path", path)
		if err := reloadSettings(path); err != nil {
			return fmt.Errorf("failed to reload settings: %w", err)
		}
		dataPath = settings.GetString(settings.DataPathKey)
		if err := reinitLogger(logLevel); err != nil {
			return fmt.Errorf("failed to reinitialize logger: %w", err)
		}
	} else {
		settings.SetReadOnly(false)
	}

	ipcServer, err := initIPC(dataPath)
	if err != nil {
		return fmt.Errorf("failed to initialize IPC server: %w", err)
	}

	if isWindowsService {
		if err := startWindowsService(); err != nil {
			ipcServer.Close()
			return fmt.Errorf("failed to start Windows service: %w", err)
		}
	} else {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
	}

	slog.Info("Shutting down...")
	t := time.AfterFunc(15*time.Second, func() {
		log.Fatal("Failed to shut down in time, forcing exit.")
	})
	defer t.Stop()
	ipcServer.Close()
	return nil
}

const tracerName = "github.com/getlantern/radiance/cmd/lanternd"

func initIPC(dataPath string) (*ipc.Server, error) {
	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"initIPC",
		trace.WithAttributes(attribute.String("dataPath", dataPath)),
	)
	defer span.End()

	span.AddEvent("initializing IPC server")

	server := ipc.NewServer(vpn.NewTunnelService(dataPath, slog.Default().With("service", "ipc"), nil))
	slog.Debug("starting IPC server")
	if err := server.Start(); err != nil {
		slog.Error("failed to start IPC server", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("start IPC server: %w", err))
	}
	return server, nil
}

func reinitLogger(level string) error {
	path := filepath.Join(settings.GetString(settings.LogPathKey), common.LogFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	lvl, _ := internal.ParseLogLevel(level)
	slog.SetDefault(internal.NewLogger(f, lvl))
	return nil
}

//go:linkname reloadSettings github.com/getlantern/radiance/common/settings.loadSettings
func reloadSettings(path string) error
