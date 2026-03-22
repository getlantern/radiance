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
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	sbjson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"go.opentelemetry.io/otel"

	box "github.com/getlantern/lantern-box"

	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/traces"
)

const (
	tracerName = "github.com/getlantern/radiance/vpn"
)

var (
	ErrTunnelNotConnected     = errors.New("tunnel not connected")
	ErrTunnelAlreadyConnected = errors.New("tunnel already connected")
)

type VPNStatus string

// Possible VPN statuses
const (
	Connecting    VPNStatus = "connecting"
	Connected     VPNStatus = "connected"
	Disconnecting VPNStatus = "disconnecting"
	Disconnected  VPNStatus = "disconnected"
	Restarting    VPNStatus = "restarting"
	ErrorStatus   VPNStatus = "error"
)

func (s *VPNStatus) String() string {
	return string(*s)
}

// VPNClient manages the lifecycle of the VPN tunnel.
type VPNClient struct {
	tunnel *tunnel

	platformIfce PlatformInterface
	logger       *slog.Logger

	preTestCancel context.CancelFunc
	preTestDone   chan struct{}

	mu sync.RWMutex
}

type PlatformInterface interface {
	libbox.PlatformInterface
	RestartService() error
	PostServiceClose()
}

// NewVPNClient creates a new VPNClient instance with the provided configuration paths, log
// level, and platform interface.
func NewVPNClient(dataPath string, logger *slog.Logger, platformIfce PlatformInterface) *VPNClient {
	if logger == nil {
		logger = slog.Default()
	}
	_ = newSplitTunnel(dataPath, logger)
	done := make(chan struct{})
	close(done)
	return &VPNClient{
		platformIfce:  platformIfce,
		logger:        logger,
		preTestCancel: func() {},
		preTestDone:   done,
	}
}

func (c *VPNClient) Connect(boxOptions BoxOptions) error {
	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"connect",
	)
	defer span.End()

	c.mu.Lock()
	// Cancel any running pre-start tests and wait for them to finish. If no tests are running,
	// preTestCancel is a no-op and preTestDone is already closed (returns immediately).
	c.preTestCancel()
	done := c.preTestDone
	c.mu.Unlock()
	<-done

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tunnel != nil {
		switch status := c.tunnel.Status(); status {
		case Connected:
			return ErrTunnelAlreadyConnected
		case Restarting, Connecting, Disconnecting:
			return fmt.Errorf("tunnel is currently %s", status)
		case Disconnected, ErrorStatus:
			// Clean up the stale tunnel so we can reconnect.
			c.tunnel = nil
		default:
			return fmt.Errorf("tunnel is in unexpected state: %s", status)
		}
	}

	options, err := buildOptions(boxOptions)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to build options: %w", err))
	}
	opts, err := sbjson.Marshal(options)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to marshal options: %w", err))
	}
	return traces.RecordError(ctx, c.start(boxOptions.BasePath, string(opts)))
}

func (c *VPNClient) start(path, options string) error {
	c.logger.Debug("Starting tunnel", "options", options)
	t := tunnel{
		dataPath: path,
	}
	if err := t.start(options, c.platformIfce); err != nil {
		return fmt.Errorf("failed to start tunnel: %w", err)
	}
	c.tunnel = &t
	return nil
}

// Close shuts down the currently running tunnel, if any. Returns an error if closing the tunnel fails.
func (c *VPNClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tunnel == nil {
		return nil
	}
	if err := c.close(); err != nil {
		return err
	}
	if c.platformIfce != nil {
		c.platformIfce.PostServiceClose()
	}
	return nil
}

func (c *VPNClient) close() error {
	t := c.tunnel
	c.tunnel = nil

	c.logger.Info("Closing tunnel")
	if err := t.close(); err != nil {
		return err
	}
	c.logger.Debug("Tunnel closed")
	runtime.GC()
	return nil
}

// Restart closes and restarts the tunnel if it is currently running. Returns an error if the tunnel
// is not running or restart fails.
func (c *VPNClient) Restart(boxOptions BoxOptions) error {
	c.mu.Lock()
	if c.tunnel == nil || c.tunnel.Status() != Connected {
		c.mu.Unlock()
		return ErrTunnelNotConnected
	}

	t := c.tunnel
	c.logger.Info("Restarting tunnel")
	t.setStatus(Restarting, nil)
	if c.platformIfce != nil {
		c.mu.Unlock()
		if err := c.platformIfce.RestartService(); err != nil {
			c.logger.Error("Failed to restart tunnel via platform interface", "error", err)
			err = fmt.Errorf("platform interface restart failed: %w", err)
			t.setStatus(ErrorStatus, err)
			return err
		}
		c.logger.Info("Tunnel restarted successfully")
		return nil
	}

	defer c.mu.Unlock()
	if err := c.close(); err != nil {
		return fmt.Errorf("closing tunnel: %w", err)
	}
	options, err := buildOptions(boxOptions)
	if err != nil {
		return fmt.Errorf("failed to build options: %w", err)
	}
	opts, err := sbjson.Marshal(options)
	if err != nil {
		return fmt.Errorf("failed to marshal options: %w", err)
	}
	if err := c.start(boxOptions.BasePath, string(opts)); err != nil {
		c.logger.Error("starting tunnel", "error", err)
		return fmt.Errorf("starting tunnel: %w", err)
	}
	c.logger.Info("Tunnel restarted successfully")
	return nil
}

// Status returns the current status of the tunnel (e.g., running, closed).
func (c *VPNClient) Status() VPNStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return Disconnected
	}
	return c.tunnel.Status()
}

// isOpen returns true if the tunnel is open, false otherwise.
// Note, this does not check if the tunnel can connect to a server.
func (c *VPNClient) isOpen() bool {
	return c.Status() == Connected
}

// Disconnect closes the tunnel and all active connections.
func (c *VPNClient) Disconnect() error {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "disconnect")
	defer span.End()
	c.logger.Info("Disconnecting VPN")
	return traces.RecordError(ctx, c.Close())
}

// SelectServer selects the specified server for the tunnel. The tunnel must already be open.
func (c *VPNClient) SelectServer(group, tag string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil || c.tunnel.Status() != Connected {
		return ErrTunnelNotConnected
	}
	return c.selectServer(c.tunnel, group, tag)
}

func (c *VPNClient) selectServer(t *tunnel, group, tag string) error {
	if group == AutoSelectTag {
		c.logger.Info("Switching to auto mode", "group", group)
		return t.selectOutbound(AutoSelectTag, "")
	}
	c.logger.Info("Selecting server", "group", group, "tag", tag)
	if err := t.selectOutbound(group, tag); err != nil {
		c.logger.Error("Failed to select server", "group", group, "tag", tag, "error", err)
		return fmt.Errorf("failed to select server %s/%s: %w", group, tag, err)
	}
	return nil
}

func (c *VPNClient) UpdateOutbounds(group string, newOptions servers.Options) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return ErrTunnelNotConnected
	}
	return c.tunnel.updateOutbounds(group, newOptions)
}

func (c *VPNClient) AddOutbounds(group string, options servers.Options) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return ErrTunnelNotConnected
	}
	return c.tunnel.addOutbounds(group, options)
}

func (c *VPNClient) RemoveOutbounds(group string, tags []string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return ErrTunnelNotConnected
	}
	return c.tunnel.removeOutbounds(group, tags)
}

// GetSelected returns the currently selected group and outbound tag.
func (c *VPNClient) GetSelected() (group, tag string, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return "", "", ErrTunnelNotConnected
	}
	outboundMgr := service.FromContext[adapter.OutboundManager](c.tunnel.ctx)
	if outboundMgr == nil {
		return "", "", errors.New("outbound manager not found")
	}
	mode := c.tunnel.clashServer.Mode()
	outbound, loaded := outboundMgr.Outbound(mode)
	if !loaded {
		return "", "", fmt.Errorf("group not found: %s", mode)
	}
	og, isGroup := outbound.(adapter.OutboundGroup)
	if !isGroup {
		return "", "", fmt.Errorf("outbound is not a group: %s", mode)
	}
	return mode, og.Now(), nil
}

func (c *VPNClient) ActiveServer() (group, tag string, err error) {
	_, span := otel.Tracer(tracerName).Start(context.Background(), "active_server")
	defer span.End()
	c.logger.Log(nil, log.LevelTrace, "Retrieving active server")
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return "", "", ErrTunnelNotConnected
	}
	outboundMgr := service.FromContext[adapter.OutboundManager](c.tunnel.ctx)
	if outboundMgr == nil {
		return "", "", errors.New("outbound manager not found")
	}
	group = c.tunnel.clashServer.Mode()
	// resolve nested groups
	tag = group
	for {
		outbound, loaded := outboundMgr.Outbound(tag)
		if !loaded {
			return group, "unavailable", fmt.Errorf("outbound not found: %s", tag)
		}
		og, isGroup := outbound.(adapter.OutboundGroup)
		if !isGroup {
			break
		}
		tag = og.Now()
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to get active server: %w", err)
	}
	return group, tag, nil
}

// Connections returns a list of all connections, both active and recently closed. A non-nil error
// is only returned if there was an error retrieving the connections, or if the tunnel is closed.
// If there are no connections and the tunnel is open, an empty slice is returned without an error.
func (c *VPNClient) Connections() ([]Connection, error) {
	_, span := otel.Tracer(tracerName).Start(context.Background(), "connections")
	defer span.End()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return nil, fmt.Errorf("failed to get connections: %w", ErrTunnelNotConnected)
	}
	tm := c.tunnel.clashServer.TrafficManager()
	activeConns := tm.Connections()
	closedConns := tm.ClosedConnections()
	connections := make([]Connection, 0, len(activeConns)+len(closedConns))
	for _, conn := range activeConns {
		connections = append(connections, newConnection(conn))
	}
	for _, conn := range closedConns {
		connections = append(connections, newConnection(conn))
	}
	return connections, nil
}

// AutoSelections represents the currently active servers for each auto server group.
type AutoSelections struct {
	Lantern string `json:"lantern"`
	User    string `json:"user"`
	AutoAll string `json:"autoAll"`
}

// AutoSelectionsEvent is emitted when server location changes for any auto server group.
type AutoSelectionsEvent struct {
	events.Event
	Selections AutoSelections `json:"selections"`
}

// AutoServerSelections returns the currently active server for each auto server group. If the group
// is not found or has no active server, "Unavailable" is returned for that group.
func (c *VPNClient) AutoServerSelections() (AutoSelections, error) {
	as := AutoSelections{
		Lantern: "Unavailable",
		User:    "Unavailable",
		AutoAll: "Unavailable",
	}
	if !c.isOpen() {
		c.logger.Log(nil, log.LevelTrace, "Tunnel not running, cannot get auto selections")
		return as, nil
	}
	groups, err := c.getGroups()
	if err != nil {
		return as, fmt.Errorf("failed to get groups: %w", err)
	}
	c.logger.Log(nil, log.LevelTrace, "Retrieved groups", "groups", groups)
	selected := func(tag string) string {
		idx := slices.IndexFunc(groups, func(g OutboundGroup) bool {
			return g.Tag == tag
		})
		if idx < 0 || groups[idx].Selected == "" {
			c.logger.Log(nil, log.LevelTrace, "Group not found or has no selection", "tag", tag)
			return "Unavailable"
		}
		return groups[idx].Selected
	}
	auto := AutoSelections{
		Lantern: selected(AutoLanternTag),
		User:    selected(AutoUserTag),
	}

	switch all := selected(AutoSelectTag); all {
	case AutoLanternTag:
		auto.AutoAll = auto.Lantern
	case AutoUserTag:
		auto.AutoAll = auto.User
	default:
		auto.AutoAll = all
	}
	return auto, nil
}

// getGroups returns all outbound groups from the outbound manager.
func (c *VPNClient) getGroups() ([]OutboundGroup, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return nil, ErrTunnelNotConnected
	}
	outboundMgr := service.FromContext[adapter.OutboundManager](c.tunnel.ctx)
	if outboundMgr == nil {
		return nil, errors.New("outbound manager not found")
	}
	var groups []OutboundGroup
	for _, it := range outboundMgr.Outbounds() {
		og, isGroup := it.(adapter.OutboundGroup)
		if !isGroup {
			continue
		}
		group := OutboundGroup{
			Tag:      og.Tag(),
			Type:     og.Type(),
			Selected: og.Now(),
		}
		for _, itemTag := range og.All() {
			itemOutbound, isLoaded := outboundMgr.Outbound(itemTag)
			if !isLoaded {
				continue
			}
			group.Outbounds = append(group.Outbounds, Outbounds{
				Tag:  itemTag,
				Type: itemOutbound.Type(),
			})
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// AutoSelectionsChangeListener returns a channel that receives a signal whenever any auto
// selection changes until the context is cancelled.
func (c *VPNClient) AutoSelectionsChangeListener(ctx context.Context) {
	go func() {
		var prev AutoSelections
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				curr, err := c.AutoServerSelections()
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

// RunOfflineURLTests will run URL tests for all outbounds if the tunnel is not currently connected.
// This can improve initial connection times by pre-determining reachability and latency to servers.
//
// If [VPNClient.Connect] is called while RunOfflineURLTests is running, the tests will be cancelled and
// any results will be discarded.
func (c *VPNClient) RunOfflineURLTests(basePath string, outbounds []option.Outbound) error {
	c.mu.Lock()
	if c.tunnel != nil {
		c.mu.Unlock()
		return ErrTunnelAlreadyConnected
	}
	select {
	case <-c.preTestDone:
		// no tests currently running, safe to start new tests
	default:
		c.mu.Unlock()
		return errors.New("pre-start tests already running")
	}
	ctx, cancel := context.WithCancel(box.BaseContext())
	c.preTestCancel = cancel
	done := make(chan struct{})
	c.preTestDone = done
	c.mu.Unlock()
	defer close(done)

	c.logger.Info("Performing pre-start URL tests")
	tags := make([]string, 0, len(outbounds))
	for _, ob := range outbounds {
		tags = append(tags, ob.Tag)
	}
	outbounds = append(outbounds, urlTestOutbound("preTest", tags))
	options := option.Options{
		Log:       &option.LogOptions{Disabled: true},
		Outbounds: outbounds,
		Experimental: &option.ExperimentalOptions{
			CacheFile: &option.CacheFileOptions{
				Enabled: true,
				Path:    filepath.Join(basePath, cacheFileName),
				CacheID: cacheID,
			},
		},
	}

	// create pre-started box instance. we just use the standard box since we don't need a
	// platform interface for testing.
	ctx = service.ContextWith[filemanager.Manager](ctx, nil)
	urlTestHistoryStorage := urltest.NewHistoryStorage()
	ctx = service.ContextWithPtr(ctx, urlTestHistoryStorage)
	service.MustRegister[adapter.URLTestHistoryStorage](ctx, urlTestHistoryStorage) // for good measure

	ctx, cancel = context.WithTimeout(ctx, 5*time.Second) // enough time for tests to complete or fail
	defer cancel()
	instance, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: options,
	})
	if err != nil {
		return fmt.Errorf("failed to create sing-box instance: %w", err)
	}
	defer instance.Close()
	// connect may have been called while we were setting up, so check if we should abort before
	// starting the instance.
	select {
	case <-ctx.Done():
		return fmt.Errorf("pre-start tests cancelled: %w", ctx.Err())
	default:
	}
	if err := instance.PreStart(); err != nil {
		return fmt.Errorf("failed to start sing-box instance: %w", err)
	}
	outbound, _ := instance.Outbound().Outbound("preTest")
	tester, _ := outbound.(adapter.URLTestGroup)
	// run URL tests
	results, err := tester.URLTest(ctx)
	if err != nil {
		c.logger.Error("Pre-start URL test failed", "error", err)
		return fmt.Errorf("pre-start URL test failed: %w", err)
	}

	var fmttedResults []string
	for tag, delay := range results {
		fmttedResults = append(fmttedResults, fmt.Sprintf("%s: [%dms]", tag, delay))
	}
	c.logger.Log(nil, log.LevelTrace, "Pre-start URL test complete", "results", strings.Join(fmttedResults, "; "))
	return nil
}
