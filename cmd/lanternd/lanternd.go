package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/ipc"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slog.Info("Starting lanternd", "version", common.Version, "dataPath", dataPath)
	be, err := backend.NewLocalBackend(ctx, backend.Options{
		DataDir:  dataPath,
		LogDir:   logPath,
		LogLevel: logLevel,
	})
	if err != nil {
		log.Fatalf("Failed to create backend: %v\n", err)
	}
	user, err := be.UserData()
	if err != nil {
		log.Fatalf("Failed to get current data: %v\n", err)
	}
	if user == nil {
		if _, err := be.NewUser(ctx); err != nil {
			log.Fatalf("Failed to create new user: %v\n", err)
		}
	}

	be.Start()
	server := ipc.NewServer(be, !common.IsMobile())
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start IPC server: %v\n", err)
	}

	// Wait for a signal to gracefully shut down.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("Shutting down...")
	// Restore default signal behavior so a second signal terminates immediately.
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)

	time.AfterFunc(15*time.Second, func() {
		slog.Error("Failed to shut down in time, forcing exit")
		os.Exit(1)
	})

	cancel()
	be.Close()
	if err := server.Close(); err != nil {
		slog.Error("Error closing IPC server", "error", err)
	}
	slog.Info("Shutdown complete")
	os.Exit(0)
}
