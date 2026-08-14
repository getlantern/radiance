package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/service"

	box "github.com/getlantern/lantern-box"
	lblog "github.com/getlantern/lantern-box/log"
	"github.com/getlantern/lantern-box/tracker/peerconn"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/portforward"
)

// StatusEvent fires whenever the Client's session state changes — successful
// Start, user Stop, or auto-Stop on a 404 heartbeat.
type StatusEvent struct {
	events.Event
	Status Status `json:"status"`
}

// ConnectionEvent fires every time a remote client opens or closes a
// samizdat session against the local peer's inbound. Source carries the
// remote "ip:port" string; consumers (the globe view, abuse aggregation)
// extract the IP for geo-lookup or rate-limit attribution. Timestamp
// is the emit time in Unix millis; consumers that aggregate across a
// time window or that need to order events when the underlying
// dispatch is async can compare it directly.
//
//	State     +1 on accept, -1 on close
//	Source    remote peer "ip:port"
//	Timestamp emit time in Unix milliseconds
type ConnectionEvent struct {
	events.Event
	State     int    `json:"state"`
	Source    string `json:"source"`
	Timestamp int64  `json:"timestamp"`
}

// Port range chosen to minimize collision risk on the typical home network,
// not to guarantee one. 30000–50000 sits above the well-known/system range
// (0–1023) and above the ports most services use by default (web/dev/dbs
// usually <30000). It overlaps both the IANA registered range (1024–49151)
// and the OS ephemeral range on some platforms (Linux's default
// net.ipv4.ip_local_port_range starts at 32768, Windows uses 49152+), so
// a collision is still possible. AddPortMapping surfaces the conflict and
// the peer.Client caller can retry with a fresh pick.
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

// Phase is the peer.Client lifecycle stage surfaced to the UI. Granular
// enough that "Share My Connection" can render a real progress sequence
// (mapping port → registering → verifying → serving) instead of a single
// active/inactive boolean. Values are stable strings so Flutter / web
// consumers can switch on them without depending on Go enum ordering.
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhaseMappingPort Phase = "mapping_port"
	PhaseDetectingIP Phase = "detecting_ip"
	PhaseRegistering Phase = "registering"
	PhaseStartingBox Phase = "starting_proxy"
	PhaseVerifying   Phase = "verifying"
	PhaseServing     Phase = "serving"
	PhaseStopping    Phase = "stopping"
	PhaseError       Phase = "error"
)

type Status struct {
	Phase Phase `json:"phase"`
	// Error is the human-readable failure reason when Phase == PhaseError.
	// Empty for every other phase; consumers should render this only when
	// the UI is in the error state.
	Error string `json:"error,omitempty"`
	// Active is true only when Phase == PhaseServing. Kept distinct from
	// Phase so subscribers that just want a boolean "is sharing?" don't
	// have to switch on the phase enum.
	Active       bool      `json:"active"`
	SharingSince time.Time `json:"sharing_since,omitempty"`
	ExternalIP   string    `json:"external_ip,omitempty"`
	ExternalPort uint16    `json:"external_port,omitempty"`
	RouteID      string    `json:"route_id,omitempty"`
}

// Config plumbs in dependencies. Zero-valued fields fall back to production
// defaults; HeartbeatInterval, HeartbeatTimeout, and CredRotationInterval
// exist so tests can drive the loops without sleeping a full minute / hour.
type Config struct {
	API                  *API
	NewForwarder         func(ctx context.Context) (portForwarder, error)
	BuildBoxService      boxFactory
	HeartbeatInterval    time.Duration
	HeartbeatTimeout     time.Duration
	CredRotationInterval time.Duration
}

// Client orchestrates one peer-proxy session: open UPnP port → register with
// lantern-cloud → run a sing-box samizdat inbound on the forwarded port →
// heartbeat → on shutdown: deregister + close inbound + unmap.
//
// Re-Starting a stopped Client is allowed.
type Client struct {
	cfg Config

	mu sync.Mutex
	// startingDone is created when Start sets starting=true and closed when
	// the same Start clears it (success or fail). Stop callers that arrive
	// mid-Start block on this channel rather than racing the in-flight
	// setup. Nil whenever no Start is in flight.
	startingDone chan struct{}
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

	// listenerDraining short-circuits the peerconn listener wrapper while
	// box.Close is firing per-connection disconnect callbacks. peerconn.Notify
	// reads its registered listener under an RLock and then releases the lock
	// before invoking it, so SetListener(nil) alone races against in-flight
	// Notify calls — under load (real client traffic), Close fires N disconnect
	// callbacks from N goroutines that have already snapshotted the listener,
	// each then events.Emit spawns one more goroutine per subscriber. The
	// Flutter-side subscriber posts main-thread tasks per event, and a
	// hundred-task flood against a Flutter engine that's simultaneously
	// processing the SmC-off state change is the Flutter mutex crash we hit.
	// Setting this flag before box.Close drops the cascade inline.
	listenerDraining atomic.Bool

	// connsByIP counts open connections per remote IP so ActiveClients can
	// report distinct devices. Keyed by IP rather than the "ip:port" Source
	// because samizdat multiplexes many H2 streams over one TCP conn and a
	// client may open several, so counting events or ports would report more
	// people than are actually being helped. Guarded by its own mutex: the
	// peerconn listener fires from box's accept/close goroutines and must not
	// contend with Start/Stop holding c.mu.
	connsMu   sync.Mutex
	connsByIP map[string]int

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
// memory dumps, H2 leakage paths) to ~1h regardless of peer process
// lifetime.
//
// Cost per rotation: one API.Register + Deregister round trip, one
// libbox build + start + close cycle. Brief (~hundreds-of-ms) port-
// rebind window during the swap; samizdat clients see TCP RST and
// reconnect via the bandit. Acceptable trade-off vs. holding the same
// cred for the full peer process lifetime.
const peerCredRotationInterval = 1 * time.Hour

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
			if fwd := pickManualForwarder(); fwd != nil {
				return fwd, nil
			}
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
func (c *Client) Start(ctx context.Context) (retErr error) {
	c.mu.Lock()
	if c.active || c.starting {
		c.mu.Unlock()
		return errors.New("peer client already active")
	}
	c.starting = true
	c.startingDone = make(chan struct{})
	c.mu.Unlock()

	// Re-arm the listener wrapper. Stop / rollback flips this to true to
	// silence the disconnect cascade during box.Close; if we don't reset
	// here, a Stop→Start cycle would leave the wrapper permanently muted.
	c.listenerDraining.Store(false)

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
		done := c.startingDone
		c.startingDone = nil
		c.mu.Unlock()
		close(done) // unblocks any Stop call that arrived mid-Start
		if success {
			return
		}
		// A fresh ctx — the caller's may already be canceled by the time we
		// roll back, which would skip Deregister and UnmapPort and leak the
		// registered route + router rule.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), peerCleanupTimeout)
		defer cancel()
		// Always clear the connection listener on rollback. The listener is
		// only Set on the success path, so this is a no-op if Start failed
		// before reaching it — but cheap insurance against a future re-order
		// that registers earlier. Drain-flag first so any in-flight Notify
		// callbacks short-circuit even if SetListener races (see Stop).
		c.listenerDraining.Store(true)
		peerconn.SetListener(nil)
		c.resetConnTracking()
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
		// Surface the failure to the UI. Emitted AFTER cleanup so the UI
		// sees the error phase as the terminal state of this Start attempt,
		// not as a transient between phases. retErr carries whichever
		// fmt.Errorf the failing branch returned, which is the most
		// human-readable diagnostic we have ("map port %d: ...",
		// "register with lantern-cloud: ...", etc.).
		var errMsg string
		if retErr != nil {
			errMsg = retErr.Error()
		}
		c.emitPhase(PhaseError, errMsg)
	}()

	c.emitPhase(PhaseMappingPort, "")
	fwd, err := c.cfg.NewForwarder(ctx)
	if err != nil {
		return fmt.Errorf("discover gateway: %w", err)
	}
	internalPort := pickInternalPort()
	mapping, err := fwd.MapPort(ctx, internalPort, "Lantern Share My Connection")
	if err != nil {
		return fmt.Errorf("map port %d: %w", internalPort, err)
	}

	c.emitPhase(PhaseDetectingIP, "")
	externalIP, err := fwd.ExternalIP(ctx)
	if err != nil {
		return fmt.Errorf("get external ip: %w", err)
	}

	c.emitPhase(PhaseRegistering, "")
	regResp, err = c.cfg.API.Register(ctx, RegisterRequest{
		ExternalIP:   externalIP,
		ExternalPort: mapping.ExternalPort,
		InternalPort: mapping.InternalPort,
	})
	if err != nil {
		return fmt.Errorf("register with lantern-cloud: %w", err)
	}

	// Defence-in-depth: refuse to start the box if the server-supplied
	// launch_cfg is missing the expected abuse-handling rules. A
	// server-side regression that silently shipped an open-proxy
	// config would otherwise turn every peer in the field into one
	// until the next deploy. The peer prefers failing to share over
	// sharing unsafely.
	if err := validateAbuseRules(regResp.ServerConfig); err != nil {
		return fmt.Errorf("launch_cfg failed abuse-rule sanity check: %w", err)
	}

	// The peer's outbound traffic must bypass any TUN device the user's own
	// VPN may have installed — otherwise censored clients' traffic would
	// egress through the local user's Lantern proxy instead of their
	// residential connection, defeating the whole point of peer-sharing.
	// auto_detect_interface tells sing-box to bind outbound dials to the
	// underlying physical interface rather than whatever the OS routing
	// table picks (which would be the VPN TUN if the VPN is up).
	c.emitPhase(PhaseStartingBox, "")
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

	c.emitPhase(PhaseVerifying, "")
	// Now that sing-box is listening with the just-built creds, ask the
	// server to dial back through them. Splitting verify out of Register
	// into this explicit follow-up avoids the chicken-and-egg where the
	// server tried to verify before the peer could possibly be listening
	// (the cert/key only arrive in the Register response). Failure here
	// is fatal — the server has already deprecated the row, so the
	// deferred cleanup tears the rest of the session down.
	if err := c.cfg.API.Verify(ctx, regResp.RouteID); err != nil {
		return fmt.Errorf("verify with lantern-cloud: %w", err)
	}

	// Forward inbound accept/close events from lantern-box's samizdat
	// inbound to the radiance event bus. Consumers (lantern-core's
	// FlutterEventEmitter, future abuse aggregation) subscribe via
	// events.Subscribe[ConnectionEvent]. Listener is process-wide
	// single-active; cleared on Stop and in the rollback defer so
	// post-teardown accept-loop callbacks land on a no-op rather than
	// emit events to a torn-down consumer. Must run AFTER box.Start so
	// the accept loop is serving when notifications start flowing.
	peerconn.SetListener(func(evt peerconn.Event) {
		state, source := evt.State, evt.Source
		if c.listenerDraining.Load() {
			// Diagnostic: if Notify reaches this point but we drop because
			// the drain flag is set, that's the post-Stop racing-Notify case
			// the flag was added to silence. Logging makes its frequency
			// observable instead of "events silently vanish."
			slog.Debug("peer listener: dropping post-Stop Notify",
				"state", state, "source", source)
			return
		}
		// Per-connection breadcrumb correlates samizdat-in activity with
		// peer-connection FlutterEvents on the consumer side. Debug-level
		// so prod logs aren't flooded under real traffic and so the
		// remote ip:port doesn't land in routinely-collected client logs;
		// operators investigating "no globe arcs despite samizdat traffic"
		// can flip the level. The listener-registration line below stays
		// at Info — that's a once-per-session lifecycle event, not a
		// per-connection breadcrumb.
		slog.Debug("peer listener: forwarding connection event",
			"state", state, "source", source)
		c.trackConn(state, source)
		events.Emit(ConnectionEvent{State: state, Source: source, Timestamp: time.Now().UnixMilli()})
	})
	slog.Info("peer listener: registered with peerconn", "route_id", regResp.RouteID)

	// HeartbeatIntervalSeconds is server-driven so lantern-cloud can dial up
	// the cadence on registrations it wants to expire faster. Honor any
	// positive value verbatim — clamping short intervals up would defeat
	// that and risk the server reaping the route between our heartbeats.
	// A non-positive value means the field was unset (e.g., older server,
	// JSON omitted); fall back to a sane default.
	heartbeat := c.cfg.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = time.Duration(regResp.HeartbeatIntervalSeconds) * time.Second
		if heartbeat <= 0 {
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
		Phase:        PhaseServing,
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
//
// If a Start is in flight when Stop is called, Stop waits for that Start to
// finish (success or fail) before proceeding. Without this, a Stop arriving
// while starting=true would return nil and let the racing Start leave the
// client active afterward — exactly the orphaned-session shape Start's own
// rollback path is designed to prevent. The wait honors ctx so a cancellable
// caller still has an exit door if Start hangs.
func (c *Client) Stop(ctx context.Context) error {
	c.mu.Lock()
	for c.starting {
		done := c.startingDone
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		c.mu.Lock()
	}
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
	c.status = Status{Phase: PhaseStopping}
	stoppingSnapshot := c.status
	c.mu.Unlock()
	events.Emit(StatusEvent{Status: stoppingSnapshot})

	// Suppress the connection listener BEFORE box.Close. peerconn.Notify
	// reads its registered listener under an RLock and releases it before
	// invoking — SetListener(nil) alone races against in-flight Notify
	// goroutines that have already snapshotted the listener (one per live
	// inbound connection at Close time). Flipping listenerDraining first
	// short-circuits the wrapper inline so even the racing invocations
	// become no-ops. SetListener(nil) is still called for cleanliness and
	// to release the listener closure's reference to this Client.
	c.listenerDraining.Store(true)
	peerconn.SetListener(nil)
	c.resetConnTracking()

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
	c.mu.Lock()
	c.status = Status{Phase: PhaseIdle}
	idleSnapshot := c.status
	c.mu.Unlock()
	events.Emit(StatusEvent{Status: idleSnapshot})
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

// emitPhase updates c.status.Phase under the lock and emits a snapshot.
// Used at each lifecycle boundary in Start / Stop so the UI sees progress
// instead of a binary active/inactive flip. Active is recomputed here:
// only PhaseServing implies active=true; every other phase clears it so
// subscribers using just the Active flag don't see e.g. "active=true with
// Phase=verifying" mid-Start.
func (c *Client) emitPhase(p Phase, errMsg string) {
	c.mu.Lock()
	c.status.Phase = p
	c.status.Error = errMsg
	c.status.Active = (p == PhaseServing)
	snapshot := c.status
	c.mu.Unlock()
	events.Emit(StatusEvent{Status: snapshot})
}

// heartbeatLoop closes done on exit so Stop can wait for the loop before
// tearing down resources. The channel is passed in rather than read off the
// Client because Stop nils c.runDone before waiting on its local copy.
// trackConn maintains the per-IP open-connection tally behind ActiveClients.
// A -1 for an IP with no recorded connection is ignored rather than allowed to
// go negative: peerconn is process-wide and a disconnect for a connection
// accepted before this Client registered its listener can still arrive.
func (c *Client) trackConn(state int, source string) {
	ip, _, err := net.SplitHostPort(source)
	if err != nil {
		ip = source
	}
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	// Re-check the drain flag here, not just in the listener wrapper. A
	// callback that passed the wrapper's check before Stop set the flag can
	// reach this point after resetConnTracking has already cleared the map,
	// repopulating it and leaking a stale count into the next Start. Checking
	// under connsMu is what makes this airtight rather than merely narrower:
	// Stop sets the flag before taking connsMu to reset, so any call that
	// acquires the lock after the reset necessarily observes the flag.
	if c.listenerDraining.Load() {
		return
	}
	if c.connsByIP == nil {
		c.connsByIP = make(map[string]int)
	}
	switch {
	case state > 0:
		c.connsByIP[ip]++
	case state < 0:
		if n, ok := c.connsByIP[ip]; ok {
			if n <= 1 {
				delete(c.connsByIP, ip)
			} else {
				c.connsByIP[ip] = n - 1
			}
		}
	}
}

// ActiveClients reports how many distinct remote IPs currently hold at least
// one connection. Reported to the server on each heartbeat so it can stop
// assigning new clients to a peer that is already carrying its share.
func (c *Client) ActiveClients() int {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	return len(c.connsByIP)
}

// resetConnTracking drops the tally on teardown so a subsequent Start doesn't
// inherit connections belonging to the previous session's box.
func (c *Client) resetConnTracking() {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	c.connsByIP = nil
}

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
			err := c.cfg.API.Heartbeat(hbCtx, routeID, c.ActiveClients())
			cancel()
			if err != nil {
				// A single transient blip shouldn't kill the registration —
				// the server-side reaper will deprecate the row if heartbeats
				// stay missing past expiration, and we'll observe that on a
				// later heartbeat as a 404.
				slog.Warn("peer heartbeat failed", "err", err, "route_id", routeID)
				if isNotRegistered(err) {
					// Re-check current routeID under lock. If credRotationLoop
					// swapped routeID + deregistered the prior route between
					// our heartbeat-prepare and heartbeat-response, the 404
					// applies to a stale route and is expected, not a reason
					// to stop. Skip the auto-Stop and let the next tick
					// heartbeat the new route.
					c.mu.Lock()
					currentRouteID := c.routeID
					c.mu.Unlock()
					if currentRouteID != routeID {
						slog.Info("peer heartbeat 404 on stale route_id; rotation in flight, continuing",
							"stale_route_id", routeID, "current_route_id", currentRouteID)
						continue
					}
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
	// Non-positive interval would panic time.NewTicker. Treat it the same
	// as the zero case Start handles when CredRotationInterval is unset:
	// fall back to the default cap rather than disabling rotation
	// silently (an unset value still wants rotation; a negative value is
	// almost certainly a test/config bug).
	if interval <= 0 {
		interval = peerCredRotationInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.runRotation(ctx)
		}
	}
}

// runRotation wraps rotateCreds with a panic recover. libbox.Start can
// panic — the main tunnel start path already wraps it with recover for
// the same reason — and an unrecovered panic here would crash the host
// process during a background rotation, taking the user's main VPN
// with it. Treat a panic the same as any other rotation failure: log,
// keep the existing box serving, try again next tick.
func (c *Client) runRotation(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("peer cred rotation panicked; current creds remain in use",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := c.rotateCreds(ctx); err != nil {
		// Don't kill the loop on a single failure — current
		// box / route is still serving. Try again next tick.
		slog.Warn("peer cred rotation failed; current creds remain in use", "err", err)
	}
}

// rotateCreds atomically swaps the peer's samizdat credentials. On
// success: a fresh route_id and keypair are in use, the libbox inbound
// has been rebuilt against the new options, the prior route is
// deregistered server-side, and the FlutterEvent stream sees no gap.
//
// On failure, behavior depends on where rotation aborted:
//   - Before oldBox.Close (Register, options patch, BuildBoxService,
//     stop-raced-rotation paths) — the prior box keeps serving with
//     its existing creds; the newly-registered route (if any) is
//     deregistered via cleanupNewRoute.
//   - After oldBox.Close (startNewBoxWithRetry exhausts retries or
//     panics) — the listener is down until the next rotation tick
//     successfully rebinds. The router-side port mapping survives;
//     only the in-process listener is gone. The new route is
//     deregistered so the bandit doesn't hand its creds out for a
//     non-listening port.
//
// In both cases the router-side port mapping is preserved; only the
// in-process samizdat state changes.
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
	// Capture sessionRunCtx upfront so the final swap can detect a
	// Stop→Start cycle that happened mid-rotation. Just checking
	// c.active at the swap isn't enough: Stop clears active, then a
	// new Start can set it back to true before this rotation reaches
	// the swap, and the old rotation would clobber the new session's
	// state. The runCtx is unique per session, so a pointer-identity
	// check at the swap is sufficient.
	sessionRunCtx := c.runCtx
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
	// From here on, any error path must deregister regResp.RouteID —
	// otherwise the newly-created server-side row leaks until TTL expiry
	// and the bandit catalog may briefly hand out creds for a route
	// whose box never came up.
	cleanupNewRoute := func(reason error) {
		// Use a fresh ctx so a cancelled rotation ctx doesn't skip the
		// cleanup we just made necessary.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), peerCleanupTimeout)
		defer cancel()
		if dErr := c.cfg.API.Deregister(cleanupCtx, regResp.RouteID); dErr != nil {
			slog.Warn("deregister orphan route after rotation failure",
				"reason", reason, "err", dErr, "orphan_route_id", regResp.RouteID)
		}
	}

	// Same defence-in-depth gate Start applies, because rotation installs a
	// freshly fetched launch_cfg on an already-running peer. Validating only
	// at Start would let a server-side regression reach every long-lived peer
	// on its next hourly rotation. Failing here keeps the current, already
	// validated box serving.
	if err := validateAbuseRules(regResp.ServerConfig); err != nil {
		cleanupNewRoute(err)
		return fmt.Errorf("rotated launch_cfg failed abuse-rule sanity check: %w", err)
	}

	options, err := ensurePeerOutboundsBypassVPN(regResp.ServerConfig)
	if err != nil {
		cleanupNewRoute(err)
		return fmt.Errorf("patch sing-box options: %w", err)
	}

	c.mu.Lock()
	currentRunCtx := c.runCtx
	c.mu.Unlock()
	if currentRunCtx == nil || currentRunCtx != sessionRunCtx {
		// Stop ran (runCtx==nil), or a Stop→Start cycle replaced the
		// session (runCtx pointer differs from the one captured at the
		// top). Either way the build below would tie a libbox to the
		// wrong session; skip it and clean up the just-created route.
		cleanupNewRoute(errors.New("client stopped during rotation"))
		return errors.New("client stopped during rotation")
	}
	newBox, err := c.cfg.BuildBoxService(sessionRunCtx, options)
	if err != nil {
		cleanupNewRoute(err)
		return fmt.Errorf("build new sing-box: %w", err)
	}

	// Close old, start new. Order matters — both want the same port.
	// If newBox.Start fails after oldBox.Close, retry briefly to absorb
	// router-side TIME_WAIT / EADDRINUSE windows before giving up.
	if closeErr := oldBox.Close(); closeErr != nil {
		slog.Warn("close old box during rotation", "err", closeErr)
	}
	if err := startNewBoxWithRetry(ctx, newBox); err != nil {
		// Catastrophic: port is now unbound. Leave c.box pointing at
		// oldBox so a future Stop tries to close it (idempotent on
		// already-closed); the next rotation tick will try again. Also
		// deregister the now-orphan new route so the bandit doesn't
		// hand its creds out for a non-listening port.
		cleanupNewRoute(err)
		return fmt.Errorf("start new sing-box: %w", err)
	}

	// Final swap under lock. Re-check that the session is still the
	// one we started against. Per-session runCtx identity is a stricter
	// check than c.active alone: a Stop→Start cycle between the
	// runCtx-check above and now would have cleared active AND set it
	// back true, but the new session's runCtx differs from sessionRunCtx
	// — so we'd otherwise resurrect old-session state into the new one.
	c.mu.Lock()
	if !c.active || c.runCtx != sessionRunCtx {
		c.mu.Unlock()
		// Either Stop cleared state, or Stop→Start replaced the session.
		// Close the new box we just brought up (the current session has
		// no reference to it) and deregister the new route. Don't touch
		// the prior route: in the Stop-only case, Stop already
		// deregistered it; in the Stop→Start case, deregistering it
		// would defeat the rotation point of cutting off the old creds.
		if err := newBox.Close(); err != nil {
			slog.Warn("close new box after session changed during rotation", "err", err)
		}
		cleanupNewRoute(errors.New("session changed during rotation swap"))
		return errors.New("session changed during rotation")
	}
	c.box = newBox
	c.routeID = regResp.RouteID
	c.boxOptions = options
	c.status.RouteID = regResp.RouteID
	c.status.ExternalIP = externalIP
	c.mu.Unlock()

	// Deregister the prior route so the bandit stops handing the old
	// (now-invalid) creds to clients. Use a fresh ctx so a Stop that
	// races us between the swap above and the deregister doesn't cancel
	// the cleanup — leaving the old (now-invalid-locally) route in the
	// server catalog until TTL would defeat the rotation's stale-cred
	// cap, which is the whole point of the feature.
	deregCtx, cancelDereg := context.WithTimeout(context.Background(), peerCleanupTimeout)
	if err := c.cfg.API.Deregister(deregCtx, oldRouteID); err != nil {
		slog.Warn("deregister prior route after rotation",
			"err", err, "old_route_id", oldRouteID)
	}
	cancelDereg()

	slog.Info("peer cred rotation succeeded",
		"new_route_id", regResp.RouteID,
		"old_route_id", oldRouteID,
	)
	return nil
}

// startNewBoxWithRetry retries newBox.Start a handful of times with a
// short backoff to absorb router-side TIME_WAIT / EADDRINUSE between
// oldBox.Close releasing the port and newBox.Start re-binding it.
// Inter-attempt backoff totals 750ms (50+100+200+400 across 4 sleeps;
// no sleep after the final attempt) so a healthy rotation isn't
// delayed noticeably; the alternative is leaving the peer's listener
// down for the full rotation interval (default 1h) on a transient
// bind failure.
//
// libbox.Start can panic; convert that to an error here rather than
// letting it propagate. Without this, the recover in runRotation would
// catch the panic but only after rotateCreds' cleanupNewRoute path
// has been skipped — leaving the freshly-registered route orphaned
// and the port unbound until next rotation. Returning the panic as
// an error lets rotateCreds' deferred cleanup deregister the orphan.
func startNewBoxWithRetry(ctx context.Context, newBox boxService) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("start new sing-box panicked: %v", r)
		}
	}()
	const attempts = 5
	backoff := 50 * time.Millisecond
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := newBox.Start(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		// Skip the sleep on the final attempt — we won't try again,
		// so the wait is pure latency that would push total backoff
		// above the documented sub-1s budget.
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("start new sing-box (ctx cancelled after %d attempts): %w", i+1, lastErr)
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return fmt.Errorf("start new sing-box (%d attempts): %w", attempts, lastErr)
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

// pickManualForwarder resolves the manual port override against the
// two configured sources and returns a ManualForwarder, or nil if
// neither source supplies a valid port. The default NewForwarder
// factory in NewClient calls this first; nil means "fall through to
// UPnP discovery."
//
// Resolution order:
//
//  1. "peer_manual_port" setting (Advanced UI)
//  2. RADIANCE_PEER_EXTERNAL_PORT env var (developer / power-user)
//  3. nil — caller falls through to UPnP
//
// Persisted names are quoted so the comment stays accurate if Go
// identifiers move or rename.
//
// The setting is range-checked before casting to uint16 — a raw cast
// silently wraps negative values (-5 → 65531) and values above the
// port space (70000 → 4464), which would register a port the peer
// doesn't listen on (or, worse, one it does listen on for another
// service). Out-of-range / unparseable values are logged at Warn and
// the resolution falls through to the next source as if unset.
func pickManualForwarder() portForwarder {
	if raw := settings.GetInt(settings.PeerManualPortKey); raw != 0 {
		if raw < 1 || raw > 65535 {
			slog.Warn("ignoring out-of-range peer_manual_port setting; falling through to env / UPnP",
				"value", raw)
		} else {
			port := uint16(raw)
			slog.Info("peer client using manual port forward",
				"port", port, "source", "setting")
			return portforward.NewManualForwarder(port)
		}
	}
	if raw := env.GetString(env.PeerExternalPort); raw != "" {
		port, err := portforward.ParseManualPort(raw)
		if err != nil {
			slog.Warn("ignoring invalid "+env.PeerExternalPort.String(),
				"value", raw, "err", err)
		} else {
			slog.Info("peer client using manual port forward",
				"port", port, "source", env.PeerExternalPort.String())
			return portforward.NewManualForwarder(port)
		}
	}
	return nil
}

func pickInternalPort() uint16 {
	return uint16(internalPortMin + rand.IntN(internalPortMax-internalPortMin))
}

// We pass a nil PlatformInterface — peer-proxy inbounds don't need TUN /
// platform-VPN integration the way the main VPN tunnel does. The samizdat
// inbound is just an HTTPS server bound to a TCP port; sing-box's default
// network stack handles it.
//
// The registries newPeerBoxContext supplies are what let libbox decode the
// inbounds[0].type="samizdat" stanza from /peer/register; without them it
// fails with "missing inbound fields registry in context". They are scoped
// to this box instance, so the peer and the main tunnel coexist without
// stomping on each other.
func defaultBuildBoxService(ctx context.Context, options string) (boxService, error) {
	bs, err := libbox.NewServiceWithContext(newPeerBoxContext(ctx), options, nil)
	if err != nil {
		return nil, fmt.Errorf("libbox.NewServiceWithContext: %w", err)
	}
	return bs, nil
}

// newPeerBoxContext assembles the context for one peer box: cancellation
// from ctx, lantern-box's protocol registries and this box's log factory
// from a single captured base context.
//
// The base must be captured exactly once. box.BaseContext() builds a new
// service registry on every call, and service.MustRegister mutates
// whichever registry the context hands back, so registering through a
// wrapper that rebuilds the base per lookup writes into a registry that
// is discarded before libbox ever reads it — the registration silently
// does nothing.
func newPeerBoxContext(ctx context.Context) context.Context {
	base := box.BaseContext()
	// The peer runs a second box beside the main tunnel's. Absent its own
	// factory it keeps sing-box's stderr-only default, so this box's
	// router and dial errors never reach lantern.log — the signal that
	// explains why a peer-share verify failed. Mirrors the main tunnel's
	// registration.
	service.MustRegister[sblog.Factory](base, lblog.NewFactory(slog.Default().Handler()))
	return peerBoxContext{Context: ctx, base: base}
}

// peerBoxContext resolves Deadline/Done/Err from the embedded caller
// context so a Stop-induced cancel propagates into box internals. Values
// come from the caller first and from base only as a fallback, so anything
// the caller carries shadows base — which matters because base holds the one
// captured registry instance libbox both registers into and reads back.
type peerBoxContext struct {
	context.Context
	base context.Context
}

func (c peerBoxContext) Value(key any) any {
	if v := c.Context.Value(key); v != nil {
		return v
	}
	return c.base.Value(key)
}
