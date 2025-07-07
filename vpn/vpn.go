package vpn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/libbox"
	O "github.com/sagernet/sing-box/option"
)

const (
	cacheID       = "lantern"
	cacheFileName = "lantern.cache"
)

var (
	selectedServer atomic.Value
)

type Server struct {
	Group    string
	Tag      string
	Type     string
	Location string
}

func init() {
	// set the initial selected server to ensure no other value type can be stored
	selectedServer.Store(Server{})
}

// QuickConnect automatically connects to the best available server in the specified group. Valid
// groups are [ServerGroupLantern], [ServerGroupUser], "all", or the empty string. Using "all" or
// the empty string will connect to the best available server across all groups.
func QuickConnect(group string, platIfce libbox.PlatformInterface) error {
	switch group {
	case ServerGroupLantern:
		group = autoLantern
	case ServerGroupUser:
		group = autoUser
	case "all", "":
		group = autoAll
	default:
		return fmt.Errorf("invalid group: %s", group)
	}
	if isOpen() {
		return autoSelect(group)
	}

	if err := quickConnect(group, platIfce); err != nil {
		return fmt.Errorf("quick connect: %w", err)
	}
	if err := storeSelected(group, ""); err != nil {
		slog.Error("failed to store mode in cache file", slog.Any("error", err))
	}
	return nil
}

func quickConnect(group string, platIfce libbox.PlatformInterface) error {
	initSplitTunnel()
	opts, err := buildOptions(group)
	if err != nil {
		return fmt.Errorf("failed to build options: %w", err)
	}
	if err := openTunnel(opts, platIfce); err != nil {
		return fmt.Errorf("failed to open tunnel: %w", err)
	}
	selectedServer.Store(Server{
		Group: group,
		Tag:   "auto",
		Type:  "auto",
	})
	return nil
}

// ConnectToServer connects to a specific server identified by the group and tag. Valid groups are
// [ServerGroupLantern] and [ServerGroupUser].
func ConnectToServer(group, tag string, platIfce libbox.PlatformInterface) error {
	switch group {
	case ServerGroupLantern, ServerGroupUser:
	default:
		return fmt.Errorf("invalid group: %s", group)
	}
	if tag == "" {
		return errors.New("tag must be specified")
	}
	if isOpen() {
		return selectServer(group, tag)
	}
	if err := connectToServer(group, tag, platIfce); err != nil {
		return fmt.Errorf("connect to server %s/%s: %w", group, tag, err)
	}
	if err := storeSelected(group, tag); err != nil {
		slog.Error("failed to store selected server in cache file", slog.Any("error", err))
	}
	return nil
}

func connectToServer(group, tag string, platIfce libbox.PlatformInterface) error {
	initSplitTunnel()
	opts, err := buildOptions(autoAll)
	if err != nil {
		return fmt.Errorf("failed to build options: %w", err)
	}
	if err := openTunnel(opts, platIfce); err != nil {
		return fmt.Errorf("failed to open tunnel: %w", err)
	}
	return selectServer(group, tag)
}

// Reconnect attempts to reconnect to the last connected server.
func Reconnect(platIfce libbox.PlatformInterface) error {
	if isOpen() {
		return fmt.Errorf("tunnel is already open")
	}
	group, _, err := loadSelected()
	if err != nil {
		return fmt.Errorf("failed to load mode from cache file: %w", err)
	}
	switch group {
	case autoLantern, autoUser, autoAll:
		return quickConnect(group, platIfce)
	default:
		_, tag, err := loadSelected()
		if err != nil {
			return fmt.Errorf("failed to load selected server from cache file: %w", err)
		}
		return connectToServer(group, tag, platIfce)
	}
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

// Disconnect closes the tunnel and all active connections.
func Disconnect() error {
	err := libbox.NewStandaloneCommandClient().ServiceClose()
	if err != nil {
		return fmt.Errorf("failed to disconnect: %w", err)
	}
	return nil
}

type Status struct {
	TunnelOpen     bool
	SelectedServer Server
	ActiveServer   Server
}

func GetStatus() (Status, error) {
	// TODO: get server locations
	s := Status{
		TunnelOpen:     isOpen(),
		SelectedServer: selectedServer.Load().(Server),
	}
	active, err := activeServer()
	if err != nil {
		return s, fmt.Errorf("failed to get active server: %w", err)
	}
	s.ActiveServer = active
	return s, nil
}

type Connection struct {
	CreatedAt    time.Time
	Destination  string
	Domain       string
	Upload       int64
	Download     int64
	Outbound     string
	OutboundType string
	ChainList    []string
}

// ActiveConnections returns a list of currently active connections, ordered from newest to oldest.
// A non-nil error is only returned if there was an error retrieving the connections, or if the
// tunnel is closed. If there are no active connections and the tunnel is open, an empty slice is
// returned without an error.
func ActiveConnections() ([]Connection, error) {
	connections, err := activeConnections()
	if err != nil {
		return nil, fmt.Errorf("failed to get active connections: %w", err)
	}

	slices.SortStableFunc(connections, func(a, b Connection) int {
		return -a.CreatedAt.Compare(b.CreatedAt)
	})
	return connections, nil
}

var (
	cacheFileOnce  sync.Once
	cacheLoadError error

	cacheFile adapter.CacheFile
)

func loadCacheFile(path string) error {
	cacheFileOnce.Do(func() {
		cacheFile = cachefile.New(context.Background(), O.CacheFileOptions{
			Enabled: true,
			Path:    path,
			CacheID: cacheID,
		})
		cacheLoadError = cacheFile.Start(adapter.StartStateInitialize)
	})
	return cacheLoadError
}

func storeSelected(group, tag string) error {
	if err := loadCacheFile(cacheFileName); err != nil {
		return fmt.Errorf("load cache file: %w", err)
	}
	if err := cacheFile.StoreMode(group); err != nil {
		return fmt.Errorf("store group in cache file: %w", err)
	}
	if tag == "" {
		return nil
	}
	if err := cacheFile.StoreSelected(group, tag); err != nil {
		return fmt.Errorf("store selected tag in cache file: %w", err)
	}
	return nil
}

func loadSelected() (string, string, error) {
	if err := loadCacheFile(cacheFileName); err != nil {
		return "", "", fmt.Errorf("load cache file: %w", err)
	}
	group := cacheFile.LoadMode()
	tag := cacheFile.LoadSelected(group)
	return group, tag, nil
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
