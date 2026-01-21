package vpn

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/vpn/ipc"
)

var (
	ipcServer *ipc.Server
	ipcMu     sync.Mutex
)

// InitIPC initializes and starts the IPC server. If the server is already running, it returns the
// existing instance.
func InitIPC(dataPath, logPath, logLevel string, platformIfce libbox.PlatformInterface) (*ipc.Server, error) {
	ipcMu.Lock()
	defer ipcMu.Unlock()
	if ipcServer != nil {
		// already started
		slog.Log(nil, internal.LevelTrace, "IPC server already started")
		return ipcServer, nil
	}

	if err := common.InitReadOnly(dataPath, logPath, logLevel); err != nil {
		return nil, fmt.Errorf("initialize common package: %w", err)
	}
	if path := settings.GetString(settings.DataPathKey); path != "" && path != dataPath {
		dataPath = path
	}

	server := ipc.NewServer(NewTunnelService(dataPath, slog.Default().With("service", "ipc"), platformIfce))
	slog.Debug("starting IPC server")
	if err := server.Start(dataPath); err != nil {
		slog.Error("failed to start IPC server", "error", err)
		return nil, err
	}
	ipcServer = server
	return server, nil
}
