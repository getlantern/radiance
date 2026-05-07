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
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"

	box "github.com/getlantern/lantern-box"
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
	// Empty lets the server fill the observed IP in from r.RemoteAddr,
	// matching peer_handler's "external_ip empty → use observed" path.
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
			// Manual override short-circuits UPnP discovery entirely; see
			// env.PeerExternalPort.
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
	c.status = Status{}
	c.mu.Unlock()

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
// libbox returns "missing inbound fields registry in context".
//
// This runs in the same process as the user's VPN tunnel (vpn/tunnel.go),
// which calls libbox.Setup once at process start; the registries set
// here are scoped to this peer's box instance so the two coexist
// without stomping on each other.
func defaultBuildBoxService(_ context.Context, options string) (boxService, error) {
	bs, err := libbox.NewServiceWithContext(box.BaseContext(), options, nil)
	if err != nil {
		return nil, fmt.Errorf("libbox.NewServiceWithContext: %w", err)
	}
	return bs, nil
}
