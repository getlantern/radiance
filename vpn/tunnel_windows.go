package vpn

import (
	"errors"
	"fmt"
	"log/slog"
	runtimeDebug "runtime/debug"
	"time"

	sbx "github.com/getlantern/sing-box-extensions"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/conntrack"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	sbgroup "github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/service"
)

func openTunnel(opts option.Options, platIfce libbox.PlatformInterface) error {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance != nil {
		return errors.New("tunnel already opened")
	}

	log := slog.Default().With("component", "tunnel")
	tInstance = &tunnel{
		ctx: sbx.BoxContext(),
		log: log,
	}
	if err := tInstance.init(opts, platIfce); err != nil {
		return fmt.Errorf("initialize tunnel: %w", err)
	}
	return tInstance.start()
}

func (t *tunnel) start() (err error) {
	if err = t.lbService.Start(); err != nil {
		return fmt.Errorf("starting libbox service: %w", err)
	}
	tInstance.closers = append(tInstance.closers, t.lbService)
	defer func() {
		if err != nil {
			closeTunnel()
		}
	}()
	t.clashServer = service.FromContext[adapter.ClashServer](t.ctx).(*clashapi.Server)
	if err = t.optsFileWatcher.Start(); err != nil {
		return fmt.Errorf("starting config file watcher: %w", err)
	}
	tInstance.closers = append(tInstance.closers, t.optsFileWatcher)

	if err = t.svrFileWatcher.Start(); err != nil {
		return fmt.Errorf("starting user server file watcher: %w", err)
	}
	tInstance.closers = append(tInstance.closers, t.svrFileWatcher)

	return nil
}

func disconnect() error {
	return closeTunnel()
}

// isOpen returns true if the tunnel is open, false otherwise.
// Note, this does not check if the tunnel can connect to a server.
func isOpen() bool {
	tAccess.Lock()
	defer tAccess.Unlock()
	return tInstance != nil
}

func autoSelect(group string) error {
	tAccess.Lock()
	defer tAccess.Unlock()
	tInstance.clashServer.SetMode(group)
	return nil
}

func selectServer(group, tag string) error {
	tAccess.Lock()
	defer tAccess.Unlock()

	oGroup, err := tInstance.getOutboundGroup(group)
	if err != nil {
		return fmt.Errorf("get outbound group %s: %w", group, err)
	}
	selector := oGroup.(*sbgroup.Selector)
	if !selector.SelectOutbound(tag) {
		return fmt.Errorf("outbound %s not found in group %s", tag, group)
	}
	if group == tInstance.clashServer.Mode() {
		return nil
	}

	// Since we want to switch servers, we need to close any existing connections to the old server.
	// The Selector outbound will handle closing connections automatically, but only for connections
	// using it. If we're switching to a different group, then we have to close the connections ourselves.
	tInstance.clashServer.SetMode(group)
	conntrack.Close()
	go func() {
		time.Sleep(time.Second)
		runtimeDebug.FreeOSMemory()
	}()
	return nil
}

func activeServer() (Server, error) {
	s := Server{}
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance == nil {
		return s, nil
	}

	group := tInstance.clashServer.Mode()
	outboundMgr := service.FromContext[adapter.OutboundManager](tInstance.ctx)
	outbound, found := outboundMgr.Outbound(group)
	if !found {
		return s, fmt.Errorf("outbound group %s not found", group)
	}
	oGroup := outbound.(adapter.OutboundGroup)
	outbound, _ = outboundMgr.Outbound(oGroup.Now())
	return Server{
		Group: oGroup.Tag(),
		Tag:   outbound.Tag(),
		Type:  outbound.Type(),
	}, nil
}

func activeConnections() ([]Connection, error) {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance == nil {
		return nil, errors.New("tunnel not opened")
	}
	connections := tInstance.clashServer.TrafficManager().Connections()
	activeConns := make([]Connection, 0, len(connections))
	for _, conn := range connections {
		c := Connection{
			CreatedAt:    conn.CreatedAt,
			Destination:  conn.Metadata.Destination.String(),
			Domain:       conn.Metadata.Domain,
			Upload:       conn.Upload.Load(),
			Download:     conn.Download.Load(),
			Outbound:     conn.Outbound,
			OutboundType: conn.OutboundType,
			ChainList:    append([]string{}, conn.Chain...),
		}
		activeConns = append(activeConns, c)
	}
	return activeConns, nil
}

func getOutboundGroupTags(group string) ([]string, error) {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance == nil {
		return nil, errors.New("tunnel not opened")
	}

	oGroup, err := tInstance.getOutboundGroup(group)
	if err != nil {
		return nil, err
	}
	return oGroup.All(), nil
}

func (t *tunnel) getOutboundGroup(group string) (adapter.OutboundGroup, error) {
	outboundMgr := service.FromContext[adapter.OutboundManager](t.ctx)
	outbound, found := outboundMgr.Outbound(group)
	if !found {
		return nil, fmt.Errorf("outbound group %s not found", group)
	}
	oGroup, isGroup := outbound.(adapter.OutboundGroup)
	if !isGroup {
		return nil, fmt.Errorf("outbound %s is not a group", group)
	}
	return oGroup, nil
}
