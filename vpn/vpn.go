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
	"strings"
	"sync"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	sbjson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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

	offlineTestCancel context.CancelFunc
	offlineTestDone   chan struct{}

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
		platformIfce:      platformIfce,
		logger:            logger,
		offlineTestCancel: func() {},
		offlineTestDone:   done,
	}
}

func (c *VPNClient) Connect(boxOptions BoxOptions) error {
	ctx, span := otel.Tracer(tracerName).Start(
		context.Background(),
		"connect",
	)
	defer span.End()

	c.mu.Lock()
	// Cancel any running offline tests and wait for them to finish. If no tests are running,
	// offlineTestCancel is a no-op and offlineTestDone is already closed (returns immediately).
	c.offlineTestCancel()
	done := c.offlineTestDone
	c.mu.Unlock()
	<-done

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tunnel != nil {
		switch status := c.tunnel.Status(); status {
		case Connected:
			return ErrTunnelAlreadyConnected
		case Connecting, Disconnecting:
			return fmt.Errorf("tunnel is currently %s", status)
		case Restarting:
			// Restart() sets this status before delegating to the platform
			// (platformIfce.RestartService) and returns immediately; on Android
			// the platform may tear down the VPN service before the restart
			// completes, in which case nothing ever transitions the tunnel
			// back out of Restarting. If Connect is being called, either that
			// restart was lost or the caller wants a fresh connection anyway
			// — clean up and proceed rather than wedging the client.
			// Emit a Disconnected event so event-driven consumers transition
			// out of Restarting even if Connect fails before start() runs.
			c.logger.Warn("tunnel stuck in Restarting, cleaning up and reconnecting")
			c.tunnel.setStatus(Disconnected, nil)
			c.tunnel = nil
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

// HistoryStorage returns the tunnel's URL test history storage or nil if the tunnel is not connected.
func (c *VPNClient) HistoryStorage() adapter.URLTestHistoryStorage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return nil
	}
	return c.tunnel.urltestHistory
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

// SelectServer changes the currently selected server to the one specified by tag. If tag is AutoSelectTag,
// the tunnel will switch to auto-select mode and automatically choose the best server.
func (c *VPNClient) SelectServer(tag string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil || c.tunnel.Status() != Connected {
		return ErrTunnelNotConnected
	}
	t := c.tunnel
	if tag == AutoSelectTag {
		return c.tunnel.selectMode(AutoSelectTag)
	}

	c.logger.Info("Selecting server", "tag", tag)
	if err := t.selectOutbound(tag); err != nil {
		c.logger.Error("Failed to select server", "tag", tag, "error", err)
		return fmt.Errorf("failed to select server %s: %w", tag, err)
	}
	return nil
}

func (c *VPNClient) UpdateOutbounds(list servers.ServerList) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return ErrTunnelNotConnected
	}
	return c.tunnel.updateOutbounds(list)
}

func (c *VPNClient) AddOutbounds(list servers.ServerList) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return ErrTunnelNotConnected
	}
	return c.tunnel.addOutbounds(list)
}

func (c *VPNClient) RemoveOutbounds(tags []string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return ErrTunnelNotConnected
	}
	return c.tunnel.removeOutbounds(tags)
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

// AutoSelectedEvent is emitted when the auto-selected server changes.
type AutoSelectedEvent struct {
	events.Event
	Selected string `json:"selected"`
}

// CurrentAutoSelectedServer returns the tag of the currently auto-selected server
func (c *VPNClient) CurrentAutoSelectedServer() (string, error) {
	if !c.isOpen() {
		c.logger.Log(nil, log.LevelTrace, "Tunnel not running, cannot get auto selections")
		return "", nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tunnel == nil {
		return "", ErrTunnelNotConnected
	}
	outbound, loaded := c.tunnel.outboundMgr.Outbound(AutoSelectTag)
	if !loaded {
		return "", fmt.Errorf("auto select group not found")
	}
	return outbound.(adapter.OutboundGroup).Now(), nil
}

const (
	rapidPollInterval  = 500 * time.Millisecond
	rapidPollWindow    = 15 * time.Second
	steadyPollInterval = 10 * time.Second
)

// AutoSelectedChangeListener polls for auto-selection changes and emits an
// AutoSelectedEvent whenever the selection differs from the previous value.
// It performs an initial rapid poll to catch the first selection soon after
// tunnel connect, then settles into a slower steady-state interval.
func (c *VPNClient) AutoSelectedChangeListener(ctx context.Context) {
	go func() {
		var prev string

		// Rapid initial poll to emit the first selection promptly after connect.
		initialDeadline := time.NewTimer(rapidPollWindow)
		defer initialDeadline.Stop()
		tick := time.NewTimer(rapidPollInterval)
		defer tick.Stop()
	initial:
		for {
			select {
			case <-ctx.Done():
				return
			case <-initialDeadline.C:
				break initial
			case <-tick.C:
				curr, err := c.CurrentAutoSelectedServer()
				if err != nil {
					tick.Reset(rapidPollInterval)
					continue
				}
				if curr != prev {
					prev = curr
					events.Emit(AutoSelectedEvent{Selected: curr})
					if curr != "" {
						break initial
					}
				}
				tick.Reset(rapidPollInterval)
			}
		}

		// Drain tick before reusing for steady-state.
		if !tick.Stop() {
			select {
			case <-tick.C:
			default:
			}
		}
		tick.Reset(steadyPollInterval)

		// Steady-state polling for ongoing changes.
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				curr, err := c.CurrentAutoSelectedServer()
				if err != nil {
					tick.Reset(steadyPollInterval)
					continue
				}
				if curr != prev {
					prev = curr
					events.Emit(AutoSelectedEvent{Selected: curr})
				}
				tick.Reset(steadyPollInterval)
			}
		}
	}()
}

// RunOfflineURLTests will run URL tests for all outbounds if the tunnel is not currently connected.
// This can improve initial connection times by pre-determining reachability and latency to servers.
//
// If [VPNClient.Connect] is called while RunOfflineURLTests is running, the tests will be cancelled and
// any results will be discarded.
func (c *VPNClient) RunOfflineURLTests(basePath string, outbounds []option.Outbound, banditURLs map[string]string) (map[string]uint16, error) {
	c.mu.Lock()
	if c.tunnel != nil {
		c.mu.Unlock()
		return nil, ErrTunnelAlreadyConnected
	}
	select {
	case <-c.offlineTestDone:
		// no tests currently running, safe to start new tests
	default:
		c.mu.Unlock()
		return nil, errors.New("offline tests already running")
	}
	ctx, cancel := context.WithCancel(box.BaseContext())
	c.offlineTestCancel = cancel
	done := make(chan struct{})
	c.offlineTestDone = done
	c.mu.Unlock()
	defer close(done)

	// Extract bandit trace context for distributed tracing
	traceCtx, hasTrace := traces.ExtractBanditTraceContext(banditURLs)

	c.logger.Info("Performing offline URL tests")
	tags := make([]string, 0, len(outbounds))
	for _, ob := range outbounds {
		tags = append(tags, ob.Tag)
	}
	outbounds = append(outbounds, urlTestOutbound("offline-test", tags, banditURLs))
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

	// create offlineed box instance. we just use the standard box since we don't need a
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
		return nil, fmt.Errorf("failed to create sing-box instance: %w", err)
	}
	defer instance.Close()
	// connect may have been called while we were setting up, so check if we should abort before
	// starting the instance.
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("offline tests cancelled: %w", ctx.Err())
	default:
	}
	if err := instance.PreStart(); err != nil {
		return nil, fmt.Errorf("failed to start sing-box instance: %w", err)
	}
	outbound, _ := instance.Outbound().Outbound("offline-test")
	tester, _ := outbound.(adapter.URLTestGroup)
	// run URL tests
	results, err := tester.URLTest(ctx)
	if err != nil {
		c.logger.Error("offline URL test failed", "error", err)
		return nil, fmt.Errorf("offline URL test failed: %w", err)
	}

	// Record URL test results in a span linked to the bandit's trace.
	if hasTrace {
		_, span := otel.Tracer(tracerName).Start(traceCtx, "url_tests_complete",
			trace.WithAttributes(
				attribute.Int("bandit.test_count", len(results)),
			),
		)
		for tag, delay := range results {
			span.AddEvent("url_test_result", trace.WithAttributes(
				attribute.String("outbound", tag),
				attribute.Int("latency_ms", int(delay)),
			))
		}
		span.End()
	}

	var fmttedResults []string
	for tag, delay := range results {
		fmttedResults = append(fmttedResults, fmt.Sprintf("%s: [%dms]", tag, delay))
	}
	c.logger.Info("offline URL test complete")
	c.logger.Log(nil, log.LevelTrace, "offline URL test results", "results", strings.Join(fmttedResults, "; "))
	return results, nil
}

// ClearNetErrorState attempts to clear any error state left by a previous unclean shutdown, such
// as from a crash. No errors are returned and this fails silently.
func ClearNetErrorState() {
	options := baseOpts("")
	options = option.Options{
		DNS:      options.DNS,
		Inbounds: options.Inbounds,
		Route: &option.RouteOptions{
			AutoDetectInterface: true,
			Rules: []option.Rule{
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: option.DefaultRule{
						RawDefaultRule: option.RawDefaultRule{
							Protocol: []string{"dns"},
						},
						RuleAction: option.RuleAction{
							Action: C.RuleActionTypeHijackDNS,
						},
					},
				},
			},
		},
	}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	b, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: options,
	})
	if err != nil {
		return
	}
	defer b.Close()
	b.Start()
}
