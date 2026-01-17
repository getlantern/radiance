package vpn

import (
	"sync"

	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/radiance/vpn/ipc"
)

var (
	ipcServer *ipc.Server
	ipcMu     sync.Mutex
)

// InitIPC initializes and starts the IPC server. If the server is already running, it returns the
// existing instance.
func InitIPC(dataPath, logPath, logLevel string, provider func() libbox.PlatformInterface) (*ipc.Server, error) {
	ipcMu.Lock()
	defer ipcMu.Unlock()
	if ipcServer != nil {
		// already started
		return ipcServer, nil
	}

	var platformIfce libbox.PlatformInterface
	if provider != nil {
		platformIfce = provider()
	}
	ipcServer = ipc.NewServer(newTunnel(dataPath, logPath, logLevel, platformIfce))
	return ipcServer, ipcServer.Start(dataPath)
}
