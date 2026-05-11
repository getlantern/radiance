package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/tracker/peerconn"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/portforward"
)

// manualPortForwarder satisfies the portForwarder interface without doing
// any UPnP work. Used when env.PeerExternalPort is set.
type manualPortForwarder struct{ port uint16 }

func (m *manualPortForwarder) MapPort(_ context.Context, _ uint16, _ string) (*portforward.Mapping, error) {
	return &portforward.Mapping{
		ExternalPort: m.port,
		InternalPort: m.port,
		Method:       "manual-env",
	}, nil
}
func (m *manualPortForwarder) UnmapPort(_ context.Context) error { return nil }
func (m *manualPortForwarder) StartRenewal(_ context.Context)    {}
func (m *manualPortForwarder) ExternalIP(_ context.Context) (string, error) {
	// An empty external IP signals the server to use the address it
	// observed on the inbound request — when the user has supplied a
	// manual port but no WAN IP, the server's view is the right answer.
	return "", nil
}

// manualPort returns the parsed env.PeerExternalPort value, or 0 if unset
// or invalid.
func manualPort() uint16 {
	raw := env.GetString(env.PeerExternalPort)
	if raw == "" {
		return 0
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 || p > 65535 {
		slog.Warn("ignoring invalid "+env.PeerExternalPort.String(), "value", raw)
		return 0
	}
	return uint16(p)
}

// StatusEvent fires whenever the Client's session state changes — successful
// Start, user Stop, or auto-Stop on a 404 heartbeat.
type StatusEvent struct {
	events.Event
	Status Status `json:"status"`
}

// ConnectionEvent fires every time a remote client opens or closes a
// samizdat session against the local peer's inbound. Source carries the
// remote "ip:port" string; consumers (the globe view, abuse aggregation)
// extract the IP for geo-lookup or rate-limit attribution.
//
//   State  +1 on accept, -1 on close
//   Source remote peer "ip:port"
type ConnectionEvent struct {
	events.Event
	State  int    `json:"state"`
	Source string `json:"source"`
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
// defaults; HeartbeatInterval and HeartbeatTimeout exist so tests can drive
// the loop without sleeping a full minute.
type Config struct {
	API               *API
	NewForwarder      func(ctx context.Context) (portForwarder, error)
	BuildBoxService   boxFactory
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
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
}

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
			if p := manualPort(); p != 0 {
				slog.Info("peer client using manual port forward",
					"port", p, "env", env.PeerExternalPort.String())
				return &manualPortForwarder{port: p}, nil
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
	peerconn.SetListener(func(state int, source string) {
		if c.listenerDraining.Load() {
			return
		}
		events.Emit(ConnectionEvent{State: state, Source: source})
	})

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

	fwd.StartRenewal(runCtx)
	go c.heartbeatLoop(runCtx, heartbeat, runDone)

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
//
// box.BaseContext registers the lantern-box protocol fields registries
// (samizdat, reflex, etc.) into the ctx so libbox can decode the
// inbounds[0].type="samizdat" stanza coming back from /peer/register.
// Without it the user's ctx is missing InboundOptionsRegistry and
// libbox returns "missing inbound fields registry in context" — the
// failure mode is silent in CI because the integration tests stub
// BuildBoxService entirely; only TestDefaultBuildBoxService_DecodesSamizdatInbound
// exercises the real decode path.
//
// We wrap so libbox sees the caller's Deadline/Done (so a Stop-induced
// ctx cancel propagates to box internals) AND can still resolve the
// registry values from box.BaseContext via Value lookups.
//
// Lives in the same process as the user's main VPN tunnel, which has
// already invoked libbox.Setup at process start. The registries set
// here are scoped to this peer's box instance via context values, so
// the two coexist without stomping on each other.
func defaultBuildBoxService(ctx context.Context, options string) (boxService, error) {
	bs, err := libbox.NewServiceWithContext(boxRegistryCtx{ctx}, options, nil)
	if err != nil {
		return nil, fmt.Errorf("libbox.NewServiceWithContext: %w", err)
	}
	return bs, nil
}

// boxRegistryCtx is a context wrapper that delegates Value() lookups to
// box.BaseContext() (where lantern-box's protocol registries live) while
// keeping the caller's Deadline/Done/Err for cancellation. Without this,
// passing box.BaseContext() directly to libbox would discard the
// caller's runCtx, leaving libbox internals running past Stop.
type boxRegistryCtx struct {
	context.Context
}

func (c boxRegistryCtx) Value(key any) any {
	if v := c.Context.Value(key); v != nil {
		return v
	}
	return box.BaseContext().Value(key)
}
