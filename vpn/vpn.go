// Package vpn provides high-level management of VPN tunnels, including connecting to the best
// available server, connecting to specific servers, disconnecting, reconnecting, and querying
// tunnel status.
package vpn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	box "github.com/getlantern/lantern-box"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
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
func QuickConnect(group string, _ libbox.PlatformInterface) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"quick_connect",
		trace.WithAttributes(attribute.String("group", group)))
	defer span.End()

	switch group {
	case servers.SGLantern:
		return traces.RecordError(ctx, ConnectToServer(servers.SGLantern, autoLanternTag, nil))
	case servers.SGUser:
		return traces.RecordError(ctx, ConnectToServer(servers.SGUser, autoUserTag, nil))
	case autoAllTag, "all", "":
		if isOpen(ctx) {
			if err := ipc.SetClashMode(ctx, autoAllTag); err != nil {
				return fmt.Errorf("failed to set auto mode: %w", err)
			}
			return nil
		}
		return traces.RecordError(ctx, connect(autoAllTag, ""))
	default:
		return traces.RecordError(ctx, fmt.Errorf("invalid group: %s", group))
	}
}

// ConnectToServer connects to a specific server identified by the group and tag. Valid groups are
// [servers.SGLantern] and [servers.SGUser].
func ConnectToServer(group, tag string, _ libbox.PlatformInterface) error {
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
	return traces.RecordError(ctx, connect(group, tag))
}

func connect(group, tag string) error {
	if isOpen(context.Background()) {
		return selectServer(context.Background(), group, tag)
	}
	return ipc.StartService(context.Background(), group, tag)
}

// Reconnect attempts to reconnect to the last connected server.
func Reconnect() error {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "reconnect")
	defer span.End()

	if isOpen(ctx) {
		return traces.RecordError(ctx, fmt.Errorf("tunnel is already open"))
	}
	return traces.RecordError(ctx, connect("", ""))
}

// isOpen returns true if the tunnel is open, false otherwise.
// Note, this does not check if the tunnel can connect to a server.
func isOpen(ctx context.Context) bool {
	state, err := ipc.GetStatus(ctx)
	if err != nil {
		slog.Error("Failed to get tunnel state", "error", err)
	}
	return state == ipc.StatusRunning
}

// Disconnect closes the tunnel and all active connections.
func Disconnect() error {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "disconnect")
	defer span.End()
	slog.Info("Disconnecting VPN")
	return traces.RecordError(ctx, ipc.StopService(ctx))
}

// selectServer selects the specified server for the tunnel. The tunnel must already be open.
func selectServer(ctx context.Context, group, tag string) error {
	slog.Info("Selecting server", "group", group, "tag", tag)
	if err := ipc.SelectOutbound(ctx, group, tag); err != nil {
		slog.Error("Failed to select server", "group", group, "tag", tag, "error", err)
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
	s := Status{
		TunnelOpen: isOpen(ctx),
	}
	if !s.TunnelOpen {
		return s, nil
	}

	slog.Log(nil, internal.LevelTrace, "Tunnel is open, retrieving selected and active servers")
	group, tag, err := ipc.GetSelected(ctx)
	if err != nil {
		return s, fmt.Errorf("failed to get selected server: %w", err)
	}
	if group == autoAllTag {
		s.SelectedServer = autoAllTag
	} else {
		s.SelectedServer = tag
	}

	_, active, err := ipc.GetActiveOutbound(ctx)
	if err != nil {
		return s, fmt.Errorf("failed to get active server: %w", err)
	}
	s.ActiveServer = active
	slog.Log(nil, internal.LevelTrace, "retrieved tunnel status", "tunnelOpen", s.TunnelOpen, "selectedServer", s.SelectedServer, "activeServer", s.ActiveServer)
	return s, nil
}

func ActiveServer(ctx context.Context) (group, tag string, err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "active_server")
	defer span.End()
	slog.Log(nil, internal.LevelTrace, "Retrieving active server")
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

// Connections returns a list of all connections, both active and recently closed. A non-nil error
// is only returned if there was an error retrieving the connections, or if the tunnel is closed.
// If there are no connections and the tunnel is open, an empty slice is returned without an error.
func Connections(ctx context.Context) ([]ipc.Connection, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "connections")
	defer span.End()
	connections, err := ipc.GetConnections(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get connections: %w", err)
	}
	return connections, nil
}

// AutoSelections represents the currently active servers for each auto server group.
type AutoSelections struct {
	Lantern string
	User    string
	AutoAll string
}

// AutoSelectionsEvent is emitted when server location changes for any auto server group.
type AutoSelectionsEvent struct {
	events.Event
	Selections AutoSelections
}

// AutoServerSelections returns the currently active server for each auto server group. If the group
// is not found or has no active server, "Unavailable" is returned for that group.
func AutoServerSelections() (AutoSelections, error) {
	as := AutoSelections{
		Lantern: "Unavailable",
		User:    "Unavailable",
		AutoAll: "Unavailable",
	}
	ctx := context.Background()
	if !isOpen(ctx) {
		slog.Log(ctx, internal.LevelTrace, "Tunnel not running, cannot get auto selections")
		return as, nil
	}
	groups, err := ipc.GetGroups(ctx)
	if err != nil {
		return as, fmt.Errorf("failed to get groups: %w", err)
	}
	slog.Log(ctx, internal.LevelTrace, "Retrieved groups", "groups", groups)
	selected := func(tag string) string {
		idx := slices.IndexFunc(groups, func(g ipc.OutboundGroup) bool {
			return g.Tag == tag
		})
		if idx < 0 || groups[idx].Selected == "" {
			slog.Log(ctx, internal.LevelTrace, "Group not found or has no selection", "tag", tag)
			return "Unavailable"
		}
		return groups[idx].Selected
	}
	auto := AutoSelections{
		Lantern: selected(autoLanternTag),
		User:    selected(autoUserTag),
	}

	switch all := selected(autoAllTag); all {
	case autoLanternTag:
		auto.AutoAll = auto.Lantern
	case autoUserTag:
		auto.AutoAll = auto.User
	default:
		auto.AutoAll = all
	}
	return auto, nil
}

// AutoSelectionsChangeListener returns a channel that receives a signal whenever any auto
// selection changes until the context is cancelled.
func AutoSelectionsChangeListener(ctx context.Context) {
	go func() {
		var prev AutoSelections
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				curr, err := AutoServerSelections()
				if err != nil {
					continue
				}
				if curr != prev {
					prev = curr
					events.Emit(AutoSelectionsEvent{
						Selections: curr,
					})
				}
			}
		}
	}()
}

const urlTestHistoryFileName = "url_test_history.json"

var (
	preStartOnce sync.Once
	preStartErr  error
)

// PreStartTests performs pre-start URL tests for all outbounds defined in configs. This can improve
// initial connection times by determining reachability and latency to servers before the tunnel is
// started. PreStartTests is only performed once per application run; usually at application startup.
func PreStartTests(path string) error {
	preStartOnce.Do(func() {
		results, err := preTest(path)
		preStartErr = err
		if err != nil {
			slog.Error("Pre-start URL test failed", "error", err)
			return
		}

		var fmttedResults []string
		for tag, delay := range results {
			fmttedResults = append(fmttedResults, fmt.Sprintf("%s: [%dms]", tag, delay))
		}
		slog.Log(nil, internal.LevelTrace, "Pre-start URL test complete", "results", strings.Join(fmttedResults, "; "))
	})
	return preStartErr
}

func preTest(path string) (map[string]uint16, error) {
	slog.Info("Performing pre-start URL tests")

	confPath := filepath.Join(path, common.ConfigFileName)
	slog.Debug("Loading config file", "confPath", confPath)
	cfg, err := loadConfig(confPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	cfgOpts := cfg.Options

	slog.Debug("Loading user servers")
	userOpts, err := loadUserOptions(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load user options: %w", err)
	}

	// since we are only doing URL tests, we only need the outbounds from both configs; we skip
	// endpoints as most/all require elevated privileges to use. just using outbounds is sufficient
	// to improve initial connect times.
	outbounds := append(cfgOpts.Outbounds, userOpts.Outbounds...)
	tags := make([]string, 0, len(outbounds))
	for _, ob := range outbounds {
		tags = append(tags, ob.Tag)
	}
	outbounds = append(outbounds, urlTestOutbound("preTest", tags, cfg.BanditURLOverrides))
	options := option.Options{
		Log:       &option.LogOptions{Disabled: true},
		Outbounds: outbounds,
	}

	// create pre-started box instance. we just use the standard box since we don't need a
	// platform interface for testing.
	ctx := box.BaseContext()
	ctx = service.ContextWith[filemanager.Manager](ctx, nil)
	urlTestHistoryStorage := urltest.NewHistoryStorage()
	ctx = service.ContextWithPtr(ctx, urlTestHistoryStorage)
	service.MustRegister[adapter.URLTestHistoryStorage](ctx, urlTestHistoryStorage) // for good measure

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second) // enough time for tests to complete or fail
	defer cancel()
	instance, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: options,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create sing-box instance: %w", err)
	}
	defer instance.Close()
	if err := instance.PreStart(); err != nil {
		return nil, fmt.Errorf("failed to start sing-box instance: %w", err)
	}
	outbound, ok := instance.Outbound().Outbound("preTest")
	if !ok {
		return nil, errors.New("preTest outbound not found")
	}
	tester, ok := outbound.(adapter.URLTestGroup)
	if !ok {
		return nil, errors.New("preTest outbound is not a URLTestGroup")
	}
	// run URL tests
	results, err := tester.URLTest(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to perform URL tests: %w", err)
	}

	historyPath := filepath.Join(path, urlTestHistoryFileName)
	if err := saveURLTestResults(urlTestHistoryStorage, historyPath, results); err != nil {
		return results, fmt.Errorf("failed to save URL test results: %w", err)
	}
	return results, nil
}

func saveURLTestResults(storage *urltest.HistoryStorage, path string, results map[string]uint16) error {
	slog.Debug("Saving URL test history", "path", path)
	history := make(map[string]*adapter.URLTestHistory, len(results))
	for tag := range results {
		history[tag] = storage.LoadURLTestHistory(tag)
	}
	buf, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("failed to marshal URL test history: %w", err)
	}
	return atomicfile.WriteFile(path, buf, 0o644)
}

func loadURLTestHistory(storage *urltest.HistoryStorage, path string) error {
	slog.Debug("Loading URL test history", "path", path)
	buf, err := atomicfile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read URL test history file: %w", err)
	}

	history := make(map[string]*adapter.URLTestHistory)
	if err := json.Unmarshal(buf, &history); err != nil {
		return fmt.Errorf("failed to unmarshal URL test history: %w", err)
	}
	for tag, result := range history {
		storage.StoreURLTestHistory(tag, result)
	}
	return nil
}

func SmartRoutingEnabled() bool {
	return settings.GetBool(settings.SmartRoutingKey)
}

func SetSmartRouting(enable bool) error {
	if SmartRoutingEnabled() == enable {
		return nil
	}
	if err := settings.Set(settings.SmartRoutingKey, enable); err != nil {
		return err
	}
	slog.Info("Updated Smart-Routing", "enabled", enable)
	return restartTunnel()
}

func AdBlockEnabled() bool {
	return settings.GetBool(settings.AdBlockKey)
}

func SetAdBlock(enable bool) error {
	if AdBlockEnabled() == enable {
		return nil
	}
	if err := settings.Set(settings.AdBlockKey, enable); err != nil {
		return err
	}
	slog.Info("Updated Ad-Block", "enabled", enable)
	return restartTunnel()
}

func restartTunnel() error {
	ctx := context.Background()
	if !isOpen(ctx) {
		return nil
	}
	slog.Info("Restarting tunnel")
	if err := ipc.RestartService(ctx); err != nil {
		return fmt.Errorf("failed to restart tunnel: %w", err)
	}
	return nil
}
