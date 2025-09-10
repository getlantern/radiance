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
	"github.com/getlantern/radiance/vpn/ipc"
)

const (
	tracerName = "github.com/getlantern/radiance/vpn"
)

// QuickConnect automatically connects to the best available server in the specified group. Valid
// groups are [servers.ServerGroupLantern], [servers.ServerGroupUser], "all", or the empty string. Using "all" or
// the empty string will connect to the best available server across all groups.
func QuickConnect(group string, platIfce libbox.PlatformInterface) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"quick_connect",
		trace.WithAttributes(attribute.String("group", group)))
	defer span.End()

	switch group {
	case servers.SGLantern:
		return traces.RecordError(ctx, ConnectToServer(servers.SGLantern, autoLanternTag, platIfce))
	case servers.SGUser:
		return traces.RecordError(ctx, ConnectToServer(servers.SGUser, autoUserTag, platIfce))
	case autoAllTag, "all", "":
		if isOpen(ctx) {
			if err := ipc.SetClashMode(ctx, autoAllTag); err != nil {
				return fmt.Errorf("failed to set auto mode: %w", err)
			}
			return nil
		}

		return traces.RecordError(ctx, connect(autoAllTag, "", platIfce))
	default:
		return traces.RecordError(ctx, fmt.Errorf("invalid group: %s", group))
	}
}

// ConnectToServer connects to a specific server identified by the group and tag. Valid groups are
// [servers.SGLantern] and [servers.SGUser].
func ConnectToServer(group, tag string, platIfce libbox.PlatformInterface) error {
	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"connect_to_server",
		trace.WithAttributes(
			attribute.String("group", group),
			attribute.String("tag", tag)))
	defer span.End()

	switch group {
	case servers.SGLantern, servers.SGUser:
	default:
		return traces.RecordError(ctx, fmt.Errorf("invalid group: %s", group))
	}
	if tag == "" {
		return traces.RecordError(ctx, errors.New("tag must be specified"))
	}
	if isOpen(ctx) {
		return traces.RecordError(ctx, selectServer(ctx, group, tag))
	}
	return traces.RecordError(ctx, connect(group, tag, platIfce))
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
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "reconnect")
	defer span.End()

	if isOpen(ctx) {
		return traces.RecordError(ctx, fmt.Errorf("tunnel is already open"))
	}
	return traces.RecordError(ctx, connect("", "", platIfce))
}

// isOpen returns true if the tunnel is open, false otherwise.
// Note, this does not check if the tunnel can connect to a server.
func isOpen(ctx context.Context) bool {
	state, err := ipc.GetStatus(ctx)
	if err != nil && !strings.Contains(err.Error(), "no such file") {
		slog.Warn("Failed to get tunnel state", "error", err)
	}
	return state == ipc.StatusRunning
}

// Disconnect closes the tunnel and all active connections.
func Disconnect() error {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "disconnect")
	defer span.End()
	return traces.RecordError(ctx, ipc.CloseService(ctx))
}

// selectServer selects the specified server for the tunnel. The tunnel must already be open.
func selectServer(ctx context.Context, group, tag string) error {
	if err := ipc.SelectOutbound(ctx, group, tag); err != nil {
		return fmt.Errorf("failed to select server %s/%s: %w", group, tag, err)
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
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "get_status")
	defer span.End()
	slog.Debug("Retrieving tunnel status")
	group, selected, err := selectedServer(ctx)
	if err != nil {
		return Status{}, traces.RecordError(ctx, fmt.Errorf("failed to get selected server: %w", err))
	}
	if group == autoAllTag {
		selected = autoAllTag
	}
	s := Status{
		TunnelOpen:     isOpen(ctx),
		SelectedServer: selected,
	}
	if !s.TunnelOpen {
		return s, nil
	}

	slog.Debug("Tunnel is open, retrieving active server")
	_, active, err := ipc.GetActiveOutbound(ctx)
	if err != nil {
		return s, fmt.Errorf("failed to get active server: %w", err)
	}
	s.ActiveServer = active
	slog.Debug("Tunnel status", "tunnelOpen", s.TunnelOpen, "selectedServer", s.SelectedServer, "activeServer", s.ActiveServer)
	return s, nil
}

func selectedServer(ctx context.Context) (string, string, error) {
	slog.Log(nil, internal.LevelTrace, "Retrieving selected server")
	if group, tag, err := ipc.GetSelected(ctx); err == nil {
		if group == autoAllTag {
			return autoAllTag, autoAllTag, nil
		}
		return group, tag, nil
	}
	slog.Log(nil, internal.LevelTrace, "Tunnel not running, reading from cache file")
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

func ActiveServer(ctx context.Context) (group, tag string, err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "active_server")
	defer span.End()
	group, tag, err = ipc.GetActiveOutbound(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get active server: %w", err)
	}
	return group, tag, nil
}

// ActiveConnections returns a list of currently active connections, ordered from newest to oldest.
// A non-nil error is only returned if there was an error retrieving the connections, or if the
// tunnel is closed. If there are no active connections and the tunnel is open, an empty slice is
// returned without an error.
func ActiveConnections(ctx context.Context) ([]ipc.Connection, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "active_connections")
	defer span.End()
	connections, err := Connections(ctx)
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to get active connections: %w", err))
	}

	connections = slices.DeleteFunc(connections, func(c ipc.Connection) bool {
		return c.ClosedAt != 0
	})
	slices.SortFunc(connections, func(a, b ipc.Connection) int {
		return int(b.CreatedAt - a.CreatedAt)
	})
	return connections, nil
}

func Connections(ctx context.Context) ([]ipc.Connection, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "connections")
	defer span.End()
	connections, err := ipc.GetConnections(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get connections: %w", err)
	}
	return connections, nil
}
