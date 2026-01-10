package vpn

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn/ipc"
)

var platIfceProvider func() libbox.PlatformInterface

// closedSvc is a stub service used while the tunnel is down
type closedSvc struct{}

func (closedSvc) Ctx() context.Context          { return context.Background() }
func (closedSvc) Status() string                { return ipc.StatusClosed }
func (closedSvc) ClashServer() *clashapi.Server { return nil }
func (closedSvc) Close() error                  { return nil }

// InitIPC starts the long-lived IPC server and hooks it up to establishConnection
func InitIPC(basePath string, provider func() libbox.PlatformInterface) (*ipc.Server, error) {
	if ipcServer != nil {
		// already started
		return ipcServer, nil
	}
	platIfceProvider = provider
	if err := common.InitReadOnly(basePath); err != nil {
		slog.Error("Failed to initialize common package", "error", err)
		return nil, fmt.Errorf("initialize common package: %w", err)
	}
	ipcServer = ipc.NewServer(closedSvc{})

	return ipcServer, ipcServer.Start(basePath, func(ctx context.Context, group, tag string) (ipc.Service, error) {
		path := settings.GetString(settings.DataPathKey)
		if path == "" {
			path = basePath
		}
		slog.Info("Starting VPN tunnel via IPC", "group", group, "tag", tag, "path", path)

		_ = newSplitTunnel(path)

		opts, err := buildOptions(group, path)
		if err != nil {
			return nil, fmt.Errorf("build options: %w", err)
		}

		var pi libbox.PlatformInterface
		if platIfceProvider != nil {
			pi = platIfceProvider()
		}

		if err := establishConnection(group, tag, opts, path, pi); err != nil {
			return nil, err
		}
		return tInstance, nil
	})
}
