package vpn

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/traces"
	"github.com/getlantern/radiance/vpn/ipc"
	"github.com/getlantern/radiance/vpn/rvpn"
)

var (
	ipcServer *ipc.Server
	ipcMu     sync.Mutex
)

// InitIPC initializes and starts the IPC server. If the server is already running, it returns the
// existing instance.
func InitIPC(dataPath, logPath, logLevel string, platformIfce rvpn.PlatformInterface) (*ipc.Server, error) {
	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"initIPC",
		trace.WithAttributes(attribute.String("dataPath", dataPath)),
	)
	defer span.End()

	ipcMu.Lock()
	defer ipcMu.Unlock()
	if ipcServer != nil && !ipcServer.IsClosed() {
		// already started
		slog.Log(nil, internal.LevelTrace, "IPC server already started")
		return ipcServer, nil
	}

	span.AddEvent("initializing IPC server")

	if err := common.InitReadOnly(dataPath, logPath, logLevel); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("init common ro: %w", err))
	}
	if path := settings.GetString(settings.DataPathKey); path != "" && path != dataPath {
		dataPath = path
	}

	server := ipc.NewServer(NewTunnelService(dataPath, slog.Default().With("service", "ipc"), platformIfce))
	slog.Debug("starting IPC server")
	if err := server.Start(dataPath); err != nil {
		slog.Error("failed to start IPC server", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("start IPC server: %w", err))
	}
	ipcServer = server
	// Set the socket path in case client and server are in the same process.
	if runtime.GOOS != "windows" {
		ipc.SetSocketPath(dataPath)
	}
	return server, nil
}
