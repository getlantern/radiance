package vpn

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"
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
	initSplitTunnel()
	switch group {
	case ServerGroupLantern, ServerGroupUser, "all", "":
	default:
		return fmt.Errorf("invalid group: %s", group)
	}
	if isOpen() {
		return autoSelect(group)
	}

	opts, err := buildOptions(modeAutoAll)
	if err != nil {
		return fmt.Errorf("failed to build options for quick connect: %w", err)
	}
	if err := openTunnel(opts, platIfce); err != nil {
		return fmt.Errorf("failed to open tunnel for quick connect: %w", err)
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
	initSplitTunnel()
	switch group {
	case ServerGroupLantern, ServerGroupUser:
	default:
		return fmt.Errorf("invalid group: %s", group)
	}
	if tag == "" {
		return errors.New("tag must be specified")
	}
	if !isOpen() {
		opts, err := buildOptions(modeBlock)
		if err != nil {
			return fmt.Errorf("failed to build options: %w", err)
		}
		if err := openTunnel(opts, platIfce); err != nil {
			return fmt.Errorf("failed to open tunnel: %w", err)
		}
	}
	return selectServer(group, tag)
}

// Reconnect attempts to reconnect to the last connected server.
func Reconnect(platIfce libbox.PlatformInterface) error {
	server := selectedServer.Load().(Server)
	if server.Tag == "auto" {
		return QuickConnect(server.Group, platIfce)
	}
	return ConnectToServer(server.Group, server.Tag, platIfce)
}

func Disconnect() error {
	return disconnect()
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
