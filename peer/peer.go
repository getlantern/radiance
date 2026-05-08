package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/lantern-box/tracker/peerconn"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/portforward"
)

// StatusEvent fires whenever the Client's session state changes — successful
// Start, user Stop, or auto-Stop on a 404 heartbeat.
type StatusEvent struct {
	events.Event
	Status Status `json:"status"`
}

// Lower bound avoids well-known/registered ports; upper bound stays below the
// typical OS ephemeral range so the OS isn't likely to assign the same port
// to another local process.
const (
	internalPortMin = 30000
	internalPortMax = 50000
)

type portForwarder interface {
	MapPort(ctx context.Context, internalPort uint16, description string) (*portforward.Mapping, error)
	UnmapPort(ctx context.Context) error
	StartRenewal(ctx context.Context)
	ExternalIP(ctx context.Context) (string, error)
}

type boxService interface {
	Start() error
	Close() error
}

type boxFactory func(ctx context.Context, options string) (boxService, error)

type Status struct {
	Active       bool      `json:"active"`
	SharingSince time.Time `json:"sharing_since,omitempty"`
	ExternalIP   string    `json:"external_ip,omitempty"`
	ExternalPort uint16    `json:"external_port,omitempty"`
	RouteID      string    `json:"route_id,omitempty"`
}

// Config plumbs in dependencies. Zero-valued fields fall back to production
// defaults; HeartbeatInterval, HeartbeatTimeout, CredRotationInterval, and
// AbuseFlushInterval exist so tests can drive the loops without sleeping a
// full minute / hour.
type Config struct {
	API                  *API
	NewForwarder         func(ctx context.Context) (portForwarder, error)
	BuildBoxService      boxFactory
	HeartbeatInterval    time.Duration
	HeartbeatTimeout     time.Duration
	CredRotationInterval time.Duration
	AbuseFlushInterval   time.Duration
}

// Client orchestrates one peer-proxy session: open UPnP port → register with
// lantern-cloud → run a sing-box samizdat inbound on the forwarded port →
// heartbeat → on shutdown: deregister + close inbound + unmap.
//
// Re-Starting a stopped Client is allowed.
type Client struct {
	cfg Config

	mu sync.Mutex
	// starting and active together serialize Start: starting is true while a
	// Start call is in flight, active is true once it succeeds. Without
	// starting, two concurrent Start calls could both pass the !active check
	// and run setup in parallel — the second's state would overwrite the
	// first's, orphaning a registered route + open box that this Client can
	// no longer Stop.
	starting  bool
	active    bool
	status    Status
	cancelRun context.CancelFunc
	runDone   chan struct{}
	forwarder portForwarder
	box       boxService
	routeID   string
	// externalPort / internalPort persist the port mapping picked at
	// Start so the cred-rotation loop can re-register against the same
	// (address, port) tuple without re-probing UPnP / re-mapping. The
	// router-side mapping itself stays put across rotations; only the
	// samizdat creds and route_id rotate.
	externalPort uint16
	internalPort uint16
	// boxOptions is the fresh options string passed to BuildBoxService,
	// kept for diagnostics and so the rotation path doesn't need to
	// re-derive it from the (also-stored) box reference.
	boxOptions string
	// runCtx is captured here for the cred-rotation goroutine to bind
	// the new libbox lifetime to the same context as the original Start.
	// Stop's cancelRun() teardown still applies to the rebuilt box.
	runCtx context.Context
}

// peerCredRotationInterval bounds how long a leaked samizdat
// credential remains usable. At each tick the peer re-registers with
// lantern-cloud (new route_id, new keypair, new shortID), rebuilds the
// libbox service against the new options, and deregisters the prior
// route. Caps blast radius from credential leakage (logs, telemetry,
// memory dumps, the H2 leakage path in engineering#3440) to ~1h
// regardless of peer process lifetime.
//
// Cost per rotation: one API.Register + Deregister round trip, one
// libbox build + start + close cycle. Brief (~hundreds-of-ms) port-
// rebind window during the swap; samizdat clients see TCP RST and
// reconnect via the bandit. Acceptable trade-off vs. holding the same
// cred for the full peer process lifetime.
const peerCredRotationInterval = 1 * time.Hour

// peerAbuseFlushInterval is how often the abuse aggregator drains its
// in-memory bucket and emits an AbuseSummaryEvent. Long enough that
// the aggregation actually does meaningful summarization (vs. just
// ferrying every connection); short enough that lantern-cloud sees
// abuse signals well within the cred-rotation window.
const peerAbuseFlushInterval = 5 * time.Minute

// peerCleanupTimeout caps how long Start's rollback path waits for
// Deregister / UnmapPort. Cleanup uses a fresh Background context (not the
// caller's ctx) so an already-canceled or expired Start ctx doesn't skip
// teardown and leak the registered route or router rule.
const peerCleanupTimeout = 30 * time.Second

func NewClient(cfg Config) (*Client, error) {
	if cfg.API == nil {
		return nil, errors.New("peer: Config.API is required")
	}
	if cfg.NewForwarder == nil {
		cfg.NewForwarder = func(ctx context.Context) (portForwarder, error) {
			// Explicitly return a nil interface on error — `return
			// portforward.NewForwarder(ctx)` collapses the (*Forwarder, error)
			// pair into a typed-nil interface on failure, which then panics
			// inside the deferred cleanup's `if fwd != nil { fwd.UnmapPort... }`
			// because the nil-check passes (interface has a type) but the
			// receiver is nil. Surfacing the underlying error here lets the
			// caller see ErrNoPortForwarding instead of a runtime panic.
			fwd, err := portforward.NewForwarder(ctx)
			if err != nil {
				return nil, err
			}
			return fwd, nil
		}
	}
	if cfg.BuildBoxService == nil {
		cfg.BuildBoxService = defaultBuildBoxService
	}
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = 30 * time.Second
	}
	return &Client{cfg: cfg}, nil
}

// Start opens the peer-proxy session. On success a background heartbeat
// goroutine is running; on error any partial setup is torn down before
// returning.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.active || c.starting {
		c.mu.Unlock()
		return errors.New("peer client already active")
	}
	c.starting = true
	c.mu.Unlock()

	var (
		success   bool
		fwd       portForwarder
		regResp   *RegisterResponse
		box       boxService
		runCtx    context.Context
		cancelRun context.CancelFunc
	)
	defer func() {
		c.mu.Lock()
		c.starting = false
		c.mu.Unlock()
		if success {
			return
		}
		// A fresh ctx — the caller's may already be canceled by the time we
		// roll back, which would skip Deregister and UnmapPort and leak the
		// registered route + router rule.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), peerCleanupTimeout)
		defer cancel()
		// Always clear the connection listener on rollback. The
		// listener is registered on the success path; this is cheap
		// insurance against a future re-order that registers earlier
		// than expected.
		peerconn.SetListener(nil)
		if box != nil {
			_ = box.Close()
		}
		if cancelRun != nil {
			cancelRun()
		}
		if regResp != nil {
			_ = c.cfg.API.Deregister(cleanupCtx, regResp.RouteID)
		}
		if fwd != nil {
			_ = fwd.UnmapPort(cleanupCtx)
		}
	}()

	fwd, err := c.cfg.NewForwarder(ctx)
	if err != nil {
		return fmt.Errorf("discover gateway: %w", err)
	}
	internalPort := pickInternalPort()
	mapping, err := fwd.MapPort(ctx, internalPort, "Lantern Share My Connection")
	if err != nil {
		return fmt.Errorf("map port %d: %w", internalPort, err)
	}

	externalIP, err := fwd.ExternalIP(ctx)
	if err != nil {
		return fmt.Errorf("get external ip: %w", err)
	}
	regResp, err = c.cfg.API.Register(ctx, RegisterRequest{
		ExternalIP:   externalIP,
		ExternalPort: mapping.ExternalPort,
		InternalPort: mapping.InternalPort,
	})
	if err != nil {
		return fmt.Errorf("register with lantern-cloud: %w", err)
	}

	// The peer's outbound traffic must bypass any TUN device the user's own
	// VPN may have installed — otherwise censored clients' traffic would
	// egress through the local user's Lantern proxy instead of their
	// residential connection, defeating the whole point of peer-sharing.
	// auto_detect_interface tells sing-box to bind outbound dials to the
	// underlying physical interface rather than whatever the OS routing
	// table picks (which would be the VPN TUN if the VPN is up).
	options, err := ensurePeerOutboundsBypassVPN(regResp.ServerConfig)
	if err != nil {
		return fmt.Errorf("patch sing-box options: %w", err)
	}

	// runCtx must outlive Start, so it derives from Background() rather than
	// the caller's ctx — otherwise libbox's stored ctx would die when Start
	// returns and take the box's internal goroutines with it.
	runCtx, cancelRun = context.WithCancel(context.Background())
	box, err = c.cfg.BuildBoxService(runCtx, options)
	if err != nil {
		cancelRun()
		return fmt.Errorf("build sing-box: %w", err)
	}
	if err := box.Start(); err != nil {
		cancelRun()
		return fmt.Errorf("start sing-box: %w", err)
	}

	heartbeat := c.cfg.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = time.Duration(regResp.HeartbeatIntervalSeconds) * time.Second
		if heartbeat < time.Minute {
			heartbeat = 5 * time.Minute
		}
	}
	runDone := make(chan struct{})

	c.mu.Lock()
	c.active = true
	c.forwarder = fwd
	c.box = box
	c.routeID = regResp.RouteID
	c.externalPort = mapping.ExternalPort
	c.internalPort = mapping.InternalPort
	c.boxOptions = options
	c.runCtx = runCtx
	c.cancelRun = cancelRun
	c.runDone = runDone
	c.status = Status{
		Active:       true,
		SharingSince: time.Now(),
		ExternalIP:   externalIP,
		ExternalPort: mapping.ExternalPort,
		RouteID:      regResp.RouteID,
	}
	statusSnapshot := c.status
	c.mu.Unlock()

	rotation := c.cfg.CredRotationInterval
	if rotation == 0 {
		rotation = peerCredRotationInterval
	}

	// Wire the peer-share connection lifecycle hook to an in-memory
	// abuse aggregator that buckets per-(source IP, destination port-
	// class) and flushes a summary on the radiance event bus every
	// flushInterval. Cleared on Stop / rollback so post-teardown
	// callbacks land on a no-op rather than into a torn-down channel.
	flushInterval := c.cfg.AbuseFlushInterval
	if flushInterval == 0 {
		flushInterval = peerAbuseFlushInterval
	}
	agg := newAbuseAggregator(flushInterval)
	peerconn.SetListener(func(evt peerconn.Event) {
		if evt.State != +1 {
			return // only +1 carries destination; close events are no-op
		}
		agg.note(evt.Source, evt.Destination)
	})
	go agg.runFlushLoop(runCtx)

	fwd.StartRenewal(runCtx)
	go c.heartbeatLoop(runCtx, heartbeat, runDone)
	go c.credRotationLoop(runCtx, rotation)

	slog.Info("peer client started",
		"external_ip", externalIP,
		"external_port", mapping.ExternalPort,
		"internal_port", mapping.InternalPort,
		"method", mapping.Method,
		"route_id", regResp.RouteID,
		"heartbeat", heartbeat,
	)
	success = true
	events.Emit(StatusEvent{Status: statusSnapshot})
	return nil
}

// Stop tears down an active session. Idempotent. Blocks until the heartbeat
// goroutine has exited and all teardown calls have completed (or timed out).
func (c *Client) Stop(ctx context.Context) error {
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return nil
	}
	cancel := c.cancelRun
	done := c.runDone
	fwd := c.forwarder
	box := c.box
	routeID := c.routeID
	c.active = false
	c.cancelRun = nil
	c.runDone = nil
	c.forwarder = nil
	c.box = nil
	c.routeID = ""
	c.externalPort = 0
	c.internalPort = 0
	c.boxOptions = ""
	c.runCtx = nil
	c.status = Status{}
	c.mu.Unlock()

	// Clear the connection listener BEFORE the box close so any
	// in-flight accept callbacks land on a no-op rather than feed
	// the (about-to-be-torn-down) abuse aggregator. The aggregator's
	// flush goroutine exits when runCtx cancels via the cancel() call
	// just below.
	peerconn.SetListener(nil)

	cancel()
	<-done

	var firstErr error
	if err := c.cfg.API.Deregister(ctx, routeID); err != nil {
		firstErr = fmt.Errorf("deregister: %w", err)
		slog.Warn("peer client deregister failed (continuing teardown)", "err", err)
	}
	if err := box.Close(); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("close sing-box: %w", err)
		}
		slog.Warn("peer client sing-box close failed", "err", err)
	}
	if err := fwd.UnmapPort(ctx); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("unmap port: %w", err)
		}
		slog.Warn("peer client unmap port failed", "err", err)
	}
	slog.Info("peer client stopped", "route_id", routeID)
	events.Emit(StatusEvent{Status: Status{}})
	return firstErr
}

func (c *Client) IsActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *Client) CurrentStatus() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// heartbeatLoop closes done on exit so Stop can wait for the loop before
// tearing down resources. The channel is passed in rather than read off the
// Client because Stop nils c.runDone before waiting on its local copy.
func (c *Client) heartbeatLoop(ctx context.Context, interval time.Duration, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.mu.Lock()
			routeID := c.routeID
			c.mu.Unlock()
			if routeID == "" {
				return
			}
			hbCtx, cancel := context.WithTimeout(ctx, c.cfg.HeartbeatTimeout)
			err := c.cfg.API.Heartbeat(hbCtx, routeID)
			cancel()
			if err != nil {
				// A single transient blip shouldn't kill the registration —
				// the server-side reaper will deprecate the row if heartbeats
				// stay missing past expiration, and we'll observe that on a
				// later heartbeat as a 404.
				slog.Warn("peer heartbeat failed", "err", err, "route_id", routeID)
				if isNotRegistered(err) {
					slog.Info("peer route no longer registered server-side, stopping client")
					// Stop runs in a separate goroutine to avoid the cyclic
					// Stop → cancelRun → loop-exit deadlock.
					go func() {
						stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						_ = c.Stop(stopCtx)
					}()
					return
				}
			}
		}
	}
}

// isNotRegistered reports whether an error from the heartbeat is a 404 from
// the server (deprecated / reaped / wrong owner). On 404 the registration is
// gone and we stop ourselves; on any other error we keep trying.
func isNotRegistered(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404
}

// credRotationLoop periodically rotates the peer's samizdat credentials
// (X25519 keypair, shortID, masquerade) by re-registering with
// lantern-cloud, rebuilding the libbox inbound, and deregistering the
// prior route. Caps blast radius from credential leakage to ~interval
// regardless of peer process lifetime — see peerCredRotationInterval.
//
// Closes done is the responsibility of heartbeatLoop; this loop just
// exits when ctx is cancelled. We deliberately don't add another close
// channel: heartbeatLoop's done already gates Stop, and rotation
// failures are non-fatal (log + retry next tick), so there's nothing
// the Stop path needs to wait on from this goroutine.
func (c *Client) credRotationLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.rotateCreds(ctx); err != nil {
				// Don't kill the loop on a single failure — current
				// box / route is still serving. Try again next tick.
				slog.Warn("peer cred rotation failed; current creds remain in use", "err", err)
			}
		}
	}
}

// rotateCreds atomically swaps the peer's samizdat credentials. On
// success: a fresh route_id and keypair are in use, the libbox inbound
// has been rebuilt against the new options, the prior route is
// deregistered server-side, and the FlutterEvent stream sees no gap.
// On failure: the prior creds and box continue serving — rotation is
// best-effort. The router-side port mapping is preserved across the
// rotation; only the in-process samizdat state changes.
//
// Sequence:
//  1. Re-register with the same (externalIP, externalPort) as Start.
//  2. Patch the new server-supplied options for VPN bypass.
//  3. Build a new libbox service against the new options.
//  4. Close the old box (releases the listening port).
//  5. Start the new box (re-binds the same port, now with new creds).
//  6. Atomic swap: c.box, c.routeID, c.boxOptions point at the new box.
//  7. Best-effort deregister of the prior route_id so the bandit
//     catalog stops handing the old (now-invalid) creds to clients.
//
// Steps 4-5 leave a brief (~hundreds of ms) window where the port
// isn't bound; samizdat clients see TCP RST and reconnect. Acceptable
// trade-off vs. the security cost of holding the same cred for the
// peer process lifetime.
func (c *Client) rotateCreds(ctx context.Context) error {
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return errors.New("not active")
	}
	fwd := c.forwarder
	extPort := c.externalPort
	intPort := c.internalPort
	oldRouteID := c.routeID
	oldBox := c.box
	c.mu.Unlock()

	if fwd == nil || oldBox == nil {
		return errors.New("rotateCreds: client state inconsistent")
	}

	externalIP, err := fwd.ExternalIP(ctx)
	if err != nil {
		return fmt.Errorf("get external ip: %w", err)
	}
	regResp, err := c.cfg.API.Register(ctx, RegisterRequest{
		ExternalIP:   externalIP,
		ExternalPort: extPort,
		InternalPort: intPort,
	})
	if err != nil {
		return fmt.Errorf("re-register: %w", err)
	}
	options, err := ensurePeerOutboundsBypassVPN(regResp.ServerConfig)
	if err != nil {
		return fmt.Errorf("patch sing-box options: %w", err)
	}

	c.mu.Lock()
	runCtx := c.runCtx
	c.mu.Unlock()
	if runCtx == nil {
		// Stop happened between the unlock above and here. Skip the
		// build to avoid spinning up a libbox tied to a torn-down ctx.
		// The new register row is harmless — server-side reaper will
		// deprecate it after TTL since no heartbeat will arrive.
		return errors.New("client stopped during rotation")
	}
	newBox, err := c.cfg.BuildBoxService(runCtx, options)
	if err != nil {
		return fmt.Errorf("build new sing-box: %w", err)
	}

	// Close old, start new. Order matters — both want the same port.
	// If newBox.Start fails after oldBox.Close, we lost the listener
	// and the next heartbeat / rotation tick is the recovery point.
	if closeErr := oldBox.Close(); closeErr != nil {
		slog.Warn("close old box during rotation", "err", closeErr)
	}
	if err := newBox.Start(); err != nil {
		// Catastrophic: port is now unbound. Leave c.box pointing at
		// oldBox so a future Stop tries to close it (idempotent on
		// already-closed); the next rotation tick will try again.
		return fmt.Errorf("start new sing-box: %w", err)
	}

	c.mu.Lock()
	c.box = newBox
	c.routeID = regResp.RouteID
	c.boxOptions = options
	c.status.RouteID = regResp.RouteID
	c.mu.Unlock()

	// Deregister the prior route so the bandit stops handing the old
	// (now-invalid) creds to clients. Best-effort: the prior row will
	// expire from its TTL anyway, but explicit deregister cuts the
	// stale-creds window from up-to-TTL down to ~immediately.
	if err := c.cfg.API.Deregister(ctx, oldRouteID); err != nil {
		slog.Warn("deregister prior route after rotation",
			"err", err, "old_route_id", oldRouteID)
	}

	slog.Info("peer cred rotation succeeded",
		"new_route_id", regResp.RouteID,
		"old_route_id", oldRouteID,
	)
	return nil
}

// ensurePeerOutboundsBypassVPN guarantees the peer sing-box's outbound dials
// bind to the physical interface rather than whatever the OS routing table
// picks. Without this, when the user's own Lantern VPN is up its TUN holds
// the default route and the peer's outbound traffic — i.e. the censored
// client's destination requests — would egress through Lantern's proxy
// network instead of the user's residential connection. That defeats the
// whole point of using the user's home IP as a circumvention exit.
//
// We splice the flag into whatever sing-box options the server supplied
// rather than relying on the server-side track config to set it, since the
// VPN-bypass requirement is a property of the *client's* environment, not
// the proxy track config.
func ensurePeerOutboundsBypassVPN(options string) (string, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(options), &raw); err != nil {
		return "", fmt.Errorf("decode options: %w", err)
	}
	route, _ := raw["route"].(map[string]any)
	if route == nil {
		route = map[string]any{}
		raw["route"] = route
	}
	route["auto_detect_interface"] = true
	out, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("encode options: %w", err)
	}
	return string(out), nil
}

func pickInternalPort() uint16 {
	return uint16(internalPortMin + rand.IntN(internalPortMax-internalPortMin))
}

// We pass a nil PlatformInterface — peer-proxy inbounds don't need TUN /
// platform-VPN integration the way the main VPN tunnel does. The samizdat
// inbound is just an HTTPS server bound to a TCP port; sing-box's default
// network stack handles it.
func defaultBuildBoxService(ctx context.Context, options string) (boxService, error) {
	bs, err := libbox.NewServiceWithContext(ctx, options, nil)
	if err != nil {
		return nil, fmt.Errorf("libbox.NewServiceWithContext: %w", err)
	}
	return bs, nil
}
