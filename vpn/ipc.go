package vpn

import (
	"context"
	"fmt"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/vpn/ipc"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
)

var platIfceProvider func() libbox.PlatformInterface

// closedSvc is a stub service used while the tunnel is down
type closedSvc struct{}

func (closedSvc) Ctx() context.Context          { return context.Background() }
func (closedSvc) Status() string                { return ipc.StatusClosed }
func (closedSvc) ClashServer() *clashapi.Server { return nil }
func (closedSvc) Close() error                  { return nil }

// InitIPC starts the long-lived IPC server and hooks it up to establishConnection
func InitIPC(basePath string, provider func() libbox.PlatformInterface) error {
	if ipcServer != nil {
		// already started
		return nil
	}
	platIfceProvider = provider
	ipcServer = ipc.NewServer(closedSvc{})
	// start tunnel via IPC. How /service/start brings the tunnel up
	ipcServer.SetStartFn(func(ctx context.Context, group, tag string) (ipc.Service, error) {
		path := basePath
		if path == "" {
			path = common.DataPath()
		}

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
	return ipcServer.Start(basePath)
}
