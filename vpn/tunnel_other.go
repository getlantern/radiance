//go:build !windows

package vpn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	sbx "github.com/getlantern/sing-box-extensions"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
)

var (
	cmdSvr     *libbox.CommandServer
	cmdSvrOnce sync.Once
)

func openTunnel(opts O.Options, platIfce libbox.PlatformInterface) error {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance != nil {
		return errors.New("tunnel already opened")
	}

	log := slog.Default().With("component", "tunnel")

	cmdSvrOnce.Do(func() {
		cmdSvr = libbox.NewCommandServer(&cmdSvrHandler{log: log}, 64)
		if err := cmdSvr.Start(); err != nil {
			log.Error("failed to start command server", slog.Any("error", err))
		}
	})

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
	// we're using the cmd server to handle libbox.Close, so we don't need to add it to closers
	defer func() {
		if err != nil {
			t.lbService.Close()
			closeTunnel()
		}
	}()
	t.clashServer = service.FromContext[adapter.ClashServer](t.ctx).(*clashapi.Server)
	cmdSvr.SetService(t.lbService)

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
	err := libbox.NewStandaloneCommandClient().ServiceClose()
	if err != nil {
		return fmt.Errorf("failed to disconnect: %w", err)
	}
	return nil
}

// isOpen returns true if the tunnel is open, false otherwise.
// Note, this does not check if the tunnel can connect to a server.
func isOpen() bool {
	res, _ := sendCmd(libbox.CommandClashMode)
	select {
	case <-res.connected:
		return true
	default:
		return false
	}
}

func autoSelect(group string) error {
	cc := libbox.NewStandaloneCommandClient()
	if err := cc.SetClashMode(group); err != nil {
		return fmt.Errorf("failed to set mode to %s: %w", group, err)
	}
	return nil
}

func selectServer(group, tag string) error {
	cc := libbox.NewStandaloneCommandClient()
	if err := cc.SelectOutbound(group, tag); err != nil {
		return fmt.Errorf("failed to select server %s/%s: %w", group, tag, err)
	}

	// Since we want to switch servers, we need to close any existing connections to the old server.
	// The Selector outbound will handle closing connections automatically, but only for connections
	// using it. If we're switching to a different group, then we have to close the connections ourselves.
	res, _ := sendCmd(libbox.CommandClashMode)
	if res.clashMode == group {
		return nil
	}
	if err := cc.SetClashMode(group); err != nil {
		return fmt.Errorf("failed to select server %s/%s: %w", group, tag, err)
	}
	if err := cc.CloseConnections(); err != nil {
		return fmt.Errorf("failed to close previous connections: %w", err)
	}
	return nil
}

func activeServer() (Server, error) {
	s := Server{}
	if !isOpen() {
		return s, nil
	}
	res, err := sendCmd(libbox.CommandClashMode)
	if err != nil {
		return s, fmt.Errorf("failed to get active server: %w", err)
	}
	outbound, err := getOutboundGroup(res.clashMode)
	if err != nil {
		return s, err
	}
	s.Group = outbound.Tag
	s.Tag = outbound.Selected
	for _, out := range outbound.ItemList {
		if out.Tag == s.Tag {
			s.Type = out.Type
			break
		}
	}

	return s, nil
}

func activeConnections() ([]Connection, error) {
	res, err := sendCmd(libbox.CommandConnections)
	if err != nil {
		return nil, fmt.Errorf("failed to get active connections: %w", err)
	}
	if res.connections == nil {
		return nil, errors.New("no active connections found")
	}
	res.connections.FilterState(libbox.ConnectionStateActive)
	var connections []Connection
	// they only provide an iterator, which stores the connections as a slice internally, and Next
	// just returns the next connection in the slice?? for real?! why tf can't they just provide a
	// the slice.. -_-
	iter := res.connections.Iterator()
	for iter.HasNext() {
		lbconn := *(iter.Next())
		conn := Connection{
			CreatedAt:    time.UnixMilli(lbconn.CreatedAt),
			Destination:  lbconn.Destination,
			Domain:       lbconn.Domain,
			Upload:       lbconn.Uplink,
			Download:     lbconn.Downlink,
			Outbound:     lbconn.Outbound,
			OutboundType: lbconn.OutboundType,
			ChainList:    append([]string{}, lbconn.ChainList...),
		}
		connections = append(connections, conn)
	}
	return connections, nil
}

func getOutboundGroupTags(group string) ([]string, error) {
	if group != ServerGroupLantern && group != ServerGroupUser {
		return nil, fmt.Errorf("invalid group: %s", group)
	}

	oGroup, err := getOutboundGroup(group)
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0)
	for _, out := range oGroup.ItemList {
		tags = append(tags, out.Tag)
	}
	return tags, nil
}

func getOutboundGroup(group string) (*libbox.OutboundGroup, error) {
	res, err := sendCmd(libbox.CommandGroup)
	if err != nil {
		return nil, fmt.Errorf("failed to get outbound groups: %w", err)
	}
	groups := res.groups
	for groups.HasNext() {
		og := groups.Next()
		if og.Tag == group {
			return og, nil
		}
	}
	return nil, fmt.Errorf("outbound group %s not found", group)
}

type cmdSvrHandler struct {
	libbox.CommandServerHandler
	log *slog.Logger
}

func (c *cmdSvrHandler) PostServiceClose() {
	if err := closeTunnel(); err != nil {
		c.log.Error("closing tunnel", slog.Any("error", err))
	}
}

func sendCmd(cmd int32) (*cmdClientHandler, error) {
	handler := newCmdClientHandler()
	opts := libbox.CommandClientOptions{Command: cmd}
	cc := libbox.NewCommandClient(handler, &opts)
	if err := cc.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to command client: %w", err)
	}
	defer cc.Disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	select {
	case <-handler.done:
		return handler, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type cmdClientHandler struct {
	status      *libbox.StatusMessage
	connections *libbox.Connections
	clashMode   string
	groups      libbox.OutboundGroupIterator
	connected   chan struct{}
	done        chan struct{}
}

func newCmdClientHandler() *cmdClientHandler {
	return &cmdClientHandler{
		connected: make(chan struct{}, 1),
		done:      make(chan struct{}, 1),
	}
}

func (c *cmdClientHandler) Connected() {
	c.connected <- struct{}{}
}
func (c *cmdClientHandler) Disconnected(message string) {}
func (c *cmdClientHandler) WriteStatus(message *libbox.StatusMessage) {
	c.status = message
	c.done <- struct{}{}
}
func (c *cmdClientHandler) InitializeClashMode(modeList libbox.StringIterator, currentMode string) {
	c.clashMode = currentMode
	c.done <- struct{}{}
}
func (c *cmdClientHandler) UpdateClashMode(newMode string) {
	c.clashMode = newMode
	c.done <- struct{}{}
}
func (c *cmdClientHandler) WriteConnections(message *libbox.Connections) {
	c.connections = message
	c.done <- struct{}{}
}

func (c *cmdClientHandler) WriteGroups(message libbox.OutboundGroupIterator) {
	c.groups = message
	c.done <- struct{}{}
}

// Not Implemented
func (c *cmdClientHandler) ClearLogs()                                  { c.done <- struct{}{} }
func (c *cmdClientHandler) WriteLogs(messageList libbox.StringIterator) { c.done <- struct{}{} }
