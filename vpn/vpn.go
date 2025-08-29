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

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/libbox"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/traces"
	"github.com/getlantern/radiance/vpn/client"
)

const (
	tracerName = "github.com/getlantern/radiance/vpn"
	meterName  = "github.com/getlantern/radiance/vpn"
)

// QuickConnect automatically connects to the best available server in the specified group. Valid
// groups are [servers.ServerGroupLantern], [servers.ServerGroupUser], "all", or the empty string. Using "all" or
// the empty string will connect to the best available server across all groups.
func QuickConnect(group string, platIfce libbox.PlatformInterface) (err error) {
	_, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"quick_connect",
		trace.WithAttributes(attribute.String("group", group)))
	defer span.End()

	switch group {
	case servers.SGLantern:
		return traces.RecordError(span, ConnectToServer(servers.SGLantern, autoLanternTag, platIfce))
	case servers.SGUser:
		return traces.RecordError(span, ConnectToServer(servers.SGUser, autoUserTag, platIfce))
	case autoAllTag, "all", "":
		if isOpen() {
			cc := libbox.NewStandaloneCommandClient()
			if err := cc.SetClashMode(autoAllTag); err != nil {
				return traces.RecordError(span, fmt.Errorf("failed to set auto mode: %w", err))
			}
			return nil
		}

		return traces.RecordError(span, connect(autoAllTag, "", platIfce))
	default:
		return traces.RecordError(span, fmt.Errorf("invalid group: %s", group))
	}
}

// ConnectToServer connects to a specific server identified by the group and tag. Valid groups are
// [servers.SGLantern] and [servers.SGUser].
func ConnectToServer(group, tag string, platIfce libbox.PlatformInterface) error {
	_, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"connect_to_server",
		trace.WithAttributes(
			attribute.String("group", group),
			attribute.String("tag", tag)))
	defer span.End()

	switch group {
	case servers.SGLantern, servers.SGUser:
	default:
		return traces.RecordError(span, fmt.Errorf("invalid group: %s", group))
	}
	if tag == "" {
		return traces.RecordError(span, errors.New("tag must be specified"))
	}
	if isOpen() {
		return traces.RecordError(span, selectServer(group, tag))
	}
	return traces.RecordError(span, connect(group, tag, platIfce))
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
	_, span := otel.Tracer(tracerName).Start(context.Background(), "reconnect")
	defer span.End()

	if isOpen() {
		return traces.RecordError(span, fmt.Errorf("tunnel is already open"))
	}
	return traces.RecordError(span, connect("", "", platIfce))
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
	_, span := otel.Tracer(tracerName).Start(context.Background(), "disconnect")
	defer span.End()
	err := libbox.NewStandaloneCommandClient().ServiceClose()
	if err != nil {
		return traces.RecordError(span, fmt.Errorf("failed to disconnect: %w", err))
	}
	return nil
}

func selectServer(group, tag string) error {
	cc := libbox.NewStandaloneCommandClient()
	if err := cc.SelectOutbound(group, tag); err != nil {
		return fmt.Errorf("failed to select server %s/%s: %w", group, tag, err)
	}

	res, _ := client.SendCmd(libbox.CommandClashMode)
	if res.ClashMode == group {
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
	_, span := otel.Tracer(tracerName).Start(context.Background(), "get_status")
	defer span.End()
	slog.Debug("Retrieving tunnel status")
	group, selected, err := selectedServer()
	if err != nil {
		return Status{}, traces.RecordError(span, fmt.Errorf("failed to get selected server: %w", err))
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
			return s, traces.RecordError(span, fmt.Errorf("failed to get active server: %w", err))
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
		res, err := client.SendCmd(libbox.CommandClashMode)
		if err != nil {
			slog.Error("Failed to retrieve clash mode", "error", err)
			return "", "", fmt.Errorf("retrieving clashMode: %w", err)
		}
		group := res.ClashMode
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
	res, err := client.SendCmd(libbox.CommandGroup)
	if err != nil {
		return "", fmt.Errorf("sending groups cmd: %w", err)
	}
	groupMap := make(map[string]*libbox.OutboundGroup)
	for _, g := range res.Groups {
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
	res, err := client.SendCmd(libbox.CommandGroup)
	if err != nil {
		return nil, fmt.Errorf("sending groups cmd: %w", err)
	}
	for _, g := range res.Groups {
		if g.Tag == group {
			return g, nil
		}
	}
	return nil, fmt.Errorf("group not found: %s", group)
}

type Connection = libbox.Connection

// ActiveConnections returns a list of currently active connections, ordered from newest to oldest.
// A non-nil error is only returned if there was an error retrieving the connections, or if the
// tunnel is closed. If there are no active connections and the tunnel is open, an empty slice is
// returned without an error.
func ActiveConnections() ([]Connection, error) {
	connections, err := Connections()
	if err != nil {
		return nil, fmt.Errorf("failed to get active connections: %w", err)
	}

	connections = slices.DeleteFunc(connections, func(c Connection) bool {
		return c.ClosedAt != 0
	})

	return connections, nil
}

func Connections() ([]Connection, error) {
	res, err := client.SendCmd(libbox.CommandConnections)
	if err != nil {
		return nil, fmt.Errorf("failed to get connections: %w", err)
	}
	if res.Connections == nil {
		return nil, errors.New("no connections found")
	}
	res.Connections.FilterState(libbox.ConnectionStateAll)
	res.Connections.SortByDate()
	var connections []Connection
	iter := res.Connections.Iterator()
	for iter.HasNext() {
		conn := *(iter.Next())
		connections = append(connections, conn)
	}
	return connections, nil
}
