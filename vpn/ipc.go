package vpn

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn/ipc"
)

var (
	platIfceProvider func() libbox.PlatformInterface

	ipcServer *ipc.Server
	ipcMu     sync.Mutex
)

// InitIPC starts the long-lived IPC server and hooks it up to establishConnection
func InitIPC(dataPath, logPath, logLevel string, provider func() libbox.PlatformInterface) (*ipc.Server, error) {
	ipcMu.Lock()
	defer ipcMu.Unlock()
	if ipcServer != nil {
		// already started
		return ipcServer, nil
	}

	if err := common.InitReadOnly(dataPath, logPath, logLevel); err != nil {
		return nil, fmt.Errorf("initialize common package: %w", err)
	}
	if dataPath == "" {
		dataPath = settings.GetString(settings.DataPathKey)
	}

	platIfceProvider = provider
	ipcServer = ipc.NewServer()
	return ipcServer, ipcServer.Start(dataPath, func(ctx context.Context, group, tag string) (ipc.Service, error) {
		slog.Info("Starting VPN tunnel via IPC", "group", group, "tag", tag, "path", dataPath)
		_ = newSplitTunnel(dataPath)
		opts, err := buildOptions(group, dataPath)
		if err != nil {
			return nil, fmt.Errorf("build options: %w", err)
		}

		var pi libbox.PlatformInterface
		if platIfceProvider != nil {
			pi = platIfceProvider()
		}

		if err := establishConnection(group, tag, opts, dataPath, pi); err != nil {
			return nil, err
		}
		return tInstance, nil
	})
}
