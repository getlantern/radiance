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
func InitIPC(basePath string, provider func() libbox.PlatformInterface) (*ipc.Server, error) {
	ipcMu.Lock()
	defer ipcMu.Unlock()
	if ipcServer != nil {
		// already started
		return ipcServer, nil
	}
	if !common.IsWindows() && basePath != "" {
		ipc.SetSocketPath(basePath)
	}

	platformInterface := provider()
	ipcServer = ipc.NewServer()
	return ipcServer, ipcServer.Start(basePath, func(ctx context.Context, group, tag string) (ipc.Service, error) {
		// Initialize common package if not already done.
		dataPath := settings.GetString(settings.DataPathKey)
		logPath := settings.GetString(settings.LogPathKey)
		if err := common.Init(dataPath, logPath, "debug"); err != nil {
			slog.Error("Failed to initialize common package", "error", err)
			return nil, fmt.Errorf("initialize common package: %w", err)
		}
		slog.Info("Starting VPN tunnel via IPC", "group", group, "tag", tag, "path", dataPath)

		_ = newSplitTunnel(dataPath)
		opts, err := buildOptions(group, dataPath)
		if err != nil {
			return nil, fmt.Errorf("build options: %w", err)
		}
		if err := establishConnection(group, tag, opts, dataPath, platformInterface); err != nil {
			return nil, err
		}
		return tInstance, nil
	})
}
