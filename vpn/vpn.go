// Package vpn provides high-level management of VPN tunnels, including connecting to the best
// available server, connecting to specific servers, disconnecting, reconnecting, and querying
// tunnel status.
package vpn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"
)

// QuickConnect automatically connects to the best available server in the specified group. Valid
// groups are [servers.ServerGroupLantern], [servers.ServerGroupUser], "all", or the empty string. Using "all" or
// the empty string will connect to the best available server across all groups.
func QuickConnect(group string, platIfce libbox.PlatformInterface) error {
	switch group {
	case servers.SGLantern:
		return ConnectToServer(servers.SGLantern, autoLanternTag, platIfce)
	case servers.SGUser:
		return ConnectToServer(servers.SGUser, autoUserTag, platIfce)
	case "":
		group = autoAllTag
		fallthrough // fall through to handle empty group as "all"
	case "all":
		if isOpen() {
			cc := libbox.NewStandaloneCommandClient()
			if err := cc.SetClashMode(autoAllTag); err != nil {
				return fmt.Errorf("failed to set auto mode: %w", err)
			}
			return nil
		}
		return connect(group, "", platIfce)
	default:
		return fmt.Errorf("invalid group: %s", group)
	}
}

// ConnectToServer connects to a specific server identified by the group and tag. Valid groups are
// [servers.SGLantern] and [servers.SGUser].
func ConnectToServer(group, tag string, platIfce libbox.PlatformInterface) error {
	switch group {
	case servers.SGLantern, servers.SGUser:
	default:
		return fmt.Errorf("invalid group: %s", group)
	}
	if tag == "" {
		return errors.New("tag must be specified")
	}
	if isOpen() {
		return selectServer(group, tag)
	}
	return connect(group, tag, platIfce)
}

func connect(group, tag string, platIfce libbox.PlatformInterface) error {
	path := common.DataPath()
	_ = newSplitTunnel(path) // ensure split tunnel rule file exists to prevent sing-box from complaining
	opts, err := buildOptions(group, path)
	if err != nil {
		return fmt.Errorf("failed to build options: %w", err)
	}
	if err := establishConnection(group, tag, opts, path, platIfce); err != nil {
		return fmt.Errorf("failed to open tunnel: %w", err)
	}
	return nil
}

// Reconnect attempts to reconnect to the last connected server.
func Reconnect(platIfce libbox.PlatformInterface) error {
	if isOpen() {
		return fmt.Errorf("tunnel is already open")
	}
	return connect("", "", platIfce)
}

// isOpen returns true if the tunnel is open, false otherwise.
// Note, this does not check if the tunnel can connect to a server.
func isOpen() bool {
	err := libbox.NewStandaloneCommandClient().SetGroupExpand("default", false)
	if err == nil {
		return true
	}
	estr := err.Error()
	if strings.Contains(estr, "database not open") {
		slog.Warn("libbox initialized but not started")
		return false
	}
	return !strings.Contains(estr, "dial unix") &&
		!strings.Contains(estr, "service not ready")
}

// Disconnect closes the tunnel and all active connections.
func Disconnect() error {
	err := libbox.NewStandaloneCommandClient().ServiceClose()
	if err != nil {
		return fmt.Errorf("failed to disconnect: %w", err)
	}
	return nil
}

func selectServer(group, tag string) error {
	cc := libbox.NewStandaloneCommandClient()
	if err := cc.SelectOutbound(group, tag); err != nil {
		return fmt.Errorf("failed to select server %s/%s: %w", group, tag, err)
	}

	res, _ := sendCmd(libbox.CommandClashMode)
	if res.clashMode == group {
		return nil
	}
	if err := cc.SetClashMode(group); err != nil {
		return fmt.Errorf("failed to select server %s/%s: %w", group, tag, err)
	}
	// If switching to a different group, close previous connections.
	if err := cc.CloseConnections(); err != nil {
		return fmt.Errorf("failed to close previous connections: %w", err)
	}
	return nil
}

// Status represents the current status of the tunnel, including whether it is open, the selected
// server, and the active server. Active is only set if the tunnel is open.
type Status struct {
	TunnelOpen bool
	// SelectedServer is the server that is currently selected for the tunnel.
	SelectedServer string
	// ActiveServer is the server that is currently active for the tunnel. This will differ from
	// SelectedServer if using auto-select mode.
	ActiveServer string
}

func GetStatus() (Status, error) {
	slog.Debug("Retrieving tunnel status")
	group, selected, err := selectedServer()
	if err != nil {
		return Status{}, fmt.Errorf("failed to get selected server: %w", err)
	}
	if group == autoAllTag {
		selected = autoAllTag
	}
	s := Status{
		TunnelOpen:     isOpen(),
		SelectedServer: selected,
	}
	if !s.TunnelOpen {
		return s, nil
	}

	switch selected {
	case autoAllTag, autoLanternTag, autoUserTag:
		s.ActiveServer, err = activeServer(group)
		if err != nil {
			return s, fmt.Errorf("failed to get active server: %w", err)
		}
	default:
		s.ActiveServer = selected
	}
	slog.Debug("Tunnel status", "tunnelOpen", s.TunnelOpen, "selectedServer", s.SelectedServer, "activeServer", s.ActiveServer)
	return s, nil
}

func selectedServer() (string, string, error) {
	slog.Log(nil, internal.LevelTrace, "Retrieving selected server")
	if isOpen() {
		slog.Log(nil, internal.LevelTrace, "Using command client")
		res, err := sendCmd(libbox.CommandClashMode)
		if err != nil {
			slog.Error("Failed to retrieve clash mode", "error", err)
			return "", "", fmt.Errorf("retrieving clashMode: %w", err)
		}
		group := res.clashMode
		if group == autoAllTag {
			return autoAllTag, autoAllTag, nil
		}
		slog.Log(nil, internal.LevelTrace, "Retrieving outbound group", "group", group)
		outbound, err := getOutboundGroup(group)
		if err != nil {
			slog.Error("Failed to retrieve outbound group", "group", group, "error", err)
			return "", "", fmt.Errorf("retrieving outbound group %v: %w", group, err)
		}
		if outbound.Selectable {
			return group, outbound.Selected, nil
		}
		return group, outbound.Tag, nil
	}
	slog.Log(nil, internal.LevelTrace, "Reading from cache file")
	opts := baseOpts().Experimental.CacheFile
	opts.Path = filepath.Join(common.DataPath(), cacheFileName)
	cacheFile := cachefile.New(context.Background(), *opts)
	if err := cacheFile.Start(adapter.StartStateInitialize); err != nil {
		return "", "", fmt.Errorf("failed to start cache file: %w", err)
	}
	group := cacheFile.LoadMode()
	tag := cacheFile.LoadSelected(group)
	// we need to ensure the cache file is closed after use or sing-box will error on start.
	cacheFile.Close()
	if group == autoAllTag {
		return "all", "auto", nil
	}
	return group, tag, nil
}

func activeServer(group string) (string, error) {
	res, err := sendCmd(libbox.CommandGroup)
	if err != nil {
		return "", fmt.Errorf("sending groups cmd: %w", err)
	}
	groupMap := make(map[string]*libbox.OutboundGroup)
	for _, g := range res.groups {
		groupMap[g.Tag] = g
	}
	if group == autoAllTag {
		if g, ok := groupMap[group]; ok {
			if g.Selected == autoLanternTag {
				group = servers.SGLantern
			} else {
				group = servers.SGUser
			}
		} else {
			if _, ok = groupMap[autoLanternTag]; ok {
				group = servers.SGLantern
			} else {
				group = servers.SGUser
			}
		}
	}
	return resolveActive(groupMap, group)
}

func resolveActive(groupMap map[string]*libbox.OutboundGroup, group string) (string, error) {
	g, ok := groupMap[group]
	if !ok {
		return "", errors.New("group not found: " + group)
	}
	selected := g.Selected
	for _, item := range g.ItemList {
		if item.Tag == selected {
			if item.Type != "urltest" {
				return item.Tag, nil
			}
			if _, ok := groupMap[item.Tag]; ok {
				return resolveActive(groupMap, item.Tag)
			}
			// urltest group missing: return first non-urltest item
			for _, i := range g.ItemList {
				if i.Type != "urltest" {
					return i.Tag, nil
				}
			}
			return "", errors.New("no non-urltest item found in group: " + group)
		}
	}
	return "", errors.New("selected item not found: " + selected)
}

func getOutboundGroup(group string) (*libbox.OutboundGroup, error) {
	res, err := sendCmd(libbox.CommandGroup)
	if err != nil {
		return nil, fmt.Errorf("sending groups cmd: %w", err)
	}
	for _, g := range res.groups {
		if g.Tag == group {
			return g, nil
		}
	}
	return nil, fmt.Errorf("group not found: %s", group)
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

func sendCmd(cmd int32) (*cmdClientHandler, error) {
	handler := newCmdClientHandler()
	opts := libbox.CommandClientOptions{Command: cmd, StatusInterval: int64(time.Second)}
	cc := libbox.NewCommandClient(handler, &opts)
	if err := cc.Connect(); err != nil {
		return nil, fmt.Errorf("connecting to command client: %w", err)
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
	groups      []*libbox.OutboundGroup
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
	groups := message
	for groups.HasNext() {
		c.groups = append(c.groups, groups.Next())
	}
	c.done <- struct{}{}
}

// Not Implemented
func (c *cmdClientHandler) ClearLogs()                                  { c.done <- struct{}{} }
func (c *cmdClientHandler) WriteLogs(messageList libbox.StringIterator) { c.done <- struct{}{} }
