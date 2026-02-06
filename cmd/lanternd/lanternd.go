package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "unsafe" // for go:linkname

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/traces"
	"github.com/getlantern/radiance/vpn"
	"github.com/getlantern/radiance/vpn/ipc"
)

var (
	dataPath = flag.String("data-path", "$HOME/.lantern", "Path to store data")
	logPath  = flag.String("log-path", "$HOME/.lantern", "Path to store logs")
	logLevel = flag.String("log-level", "info", "Logging level (trace, debug, info, warn, error)")
)

func main() {
	flag.Parse()

	dataPath := os.ExpandEnv(*dataPath)
	logPath := os.ExpandEnv(*logPath)
	logLevel := *logLevel

	slog.Info("Starting lanternd", "version", common.Version, "dataPath", dataPath)

	if err := common.Init(dataPath, logPath, logLevel); err != nil {
		log.Fatalf("Failed to initialize common: %v\n", err)
	}

	// we need to reload settings if the data path was changed via IPC. we want to keep the original
	// settings file so we know if/where to reload from next time.
	// This is temporary and will be removed once we move ownership and interaction of all files to
	// one process. maybe daemon?
	settingsPath := settings.GetString("file_path")
	if path := settings.GetString(settings.DataPathKey); path != "" && path != settingsPath {
		slog.Info("Reloading settings", "path", path)
		if err := reloadSettings(path); err != nil {
			log.Fatalf("Failed to reload settings from %s: %v\n", path, err)
		}
		settings.SetReadOnly(true)
	}

	ipcServer, err := initIPC(dataPath, logPath, logLevel)
	if err != nil {
		log.Fatalf("Failed to initialize IPC: %v\n", err)
	}

	// Wait for a signal to gracefully shut down.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("Shutting down...")
	time.AfterFunc(15*time.Second, func() {
		log.Fatal("Failed to shut down in time, forcing exit.")
	})
	ipcServer.Close()
}

const tracerName = "github.com/getlantern/radiance/cmd/lanternd"

func initIPC(dataPath, logPath, logLevel string) (*ipc.Server, error) {
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

//go:linkname reloadSettings github.com/getlantern/radiance/common/settings.loadSettings
func reloadSettings(path string) error
