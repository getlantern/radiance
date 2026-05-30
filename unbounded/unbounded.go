// Package unbounded manages the broflake / Unbounded widget-proxy lifecycle.
//
// Unbounded is the WebRTC-based donor mode for Lantern's Share My Connection
// feature: the local user contributes bandwidth to censored users via short-
// lived WebRTC sessions brokered through a discovery server, without exposing
// a long-lived inbound port the way the samizdat-over-UPnP "Share My
// Connection" mode does. It's the lower-bandwidth, lower-risk, universally-
// applicable alternative to SmC — works on networks where UPnP is disabled
// or unavailable, and the peer's residential IP isn't tied to a single
// long-lived inbound listener.
//
// Three conditions must all hold for the widget proxy to actually run:
//
//  1. settings.UnboundedKey is true (local opt-in via the UI toggle)
//  2. server-side cfg.Features[UNBOUNDED] is enabled (server says go)
//  3. server-side cfg.Unbounded provides discovery + egress URLs
//
// The manager subscribes to config.NewConfigEvent and recomputes the
// running state on every config update; it also re-evaluates when
// SetEnabled flips the local toggle. Each consumer connection change
// (accept / disconnect) emits a ConnectionEvent on the radiance event
// bus so the same Flutter globe used for SmC can render arcs without
// caring which protocol produced them.
package unbounded

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	C "github.com/getlantern/common"

	"github.com/getlantern/broflake/clientcore"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
)

// ConnectionEvent fires every time a consumer (i.e. a censored client
// being routed through this widget proxy) connects or disconnects via
// the broflake mesh.
//
//   State     +1 on accept, -1 on close
//   Source    consumer's IP if broflake exposes it, otherwise empty
//   Timestamp emit time in Unix milliseconds
//
// JSON shape is identical to peer.ConnectionEvent so a consumer
// reading both SSE streams can deserialize each frame with the
// same struct. The in-process event bus, however, keys
// subscriptions by concrete Go type, so subscribing to
// peer.ConnectionEvent does NOT also deliver unbounded
// ConnectionEvents — in-process consumers that want a unified
// view of all peer activity must subscribe to both. Broflake's
// internal worker-slot identifier is not surfaced; a consumer
// that needs to pair accept/close events for the same arc keys
// off Source (or arrival sequence within a single connection
// lifetime).
type ConnectionEvent struct {
	events.Event
	State     int    `json:"state"`
	Source    string `json:"source"`
	Timestamp int64  `json:"timestamp"`
}

var manager = &unboundedManager{}

// widget is the minimum interface the manager needs from a running
// broflake instance. Defined locally (vs using clientcore.UI) so
// tests can supply a tiny fake without implementing the full
// clientcore.UI surface area.
type widget interface {
	Stop()
}

// newWidget builds the live broflake widget. Package var so unit
// tests can swap it for a fake that records start/stop calls
// without spinning up real WebRTC.
var newWidget = func(bfOpt *clientcore.BroflakeOptions, rtcOpt *clientcore.WebRTCOptions, egOpt *clientcore.EgressOptions) (widget, error) {
	// BroflakeConn is for clients routing traffic *through* the mesh.
	// A widget proxy only donates bandwidth, so the conn is unused —
	// discard it.
	_, ui, err := clientcore.NewBroflake(bfOpt, rtcOpt, egOpt)
	if err != nil {
		return nil, err
	}
	return ui, nil
}

type unboundedManager struct {
	// transitionMu serializes start/stop. It's held for the full
	// duration of a stop (including the wait for the worker goroutine
	// to actually exit) and for the full duration of a start. Without
	// it, stop's signal-then-return path could race a concurrent start
	// — the worker is still running ui.Stop while cancel/done get
	// re-armed under a fresh worker, leaving two broflake widgets
	// alive simultaneously.
	transitionMu sync.Mutex

	// mu protects the fields below. Held only for the brief window of
	// reading or mutating manager state; never held across the wait on
	// done or any broflake call.
	mu sync.Mutex
	// armed gates every start path. InitSubscription flips it true;
	// public Stop flips it false. Without this gate, a config event
	// (or any other applyConfig caller) racing the public Stop's
	// transitionMu hold could observe cancel==nil after Stop's wait,
	// pile up at transitionMu, and start a new widget *after* the
	// shutdown caller has already returned — the LocalBackend.Close
	// docstring is explicit that Stop is the final teardown, so a
	// post-Stop revival breaks that contract. start() and applyConfig
	// re-check armed under mu (inside transitionMu for start) so a
	// concurrent flip is honored even when the caller has been queued
	// at transitionMu the whole time.
	armed  bool
	cancel context.CancelFunc
	// done is closed by the worker goroutine when it actually exits
	// (after NewBroflake returns and ui.Stop runs). stop and Stop wait
	// on this under transitionMu so backend shutdown blocks until the
	// broflake widget is actually torn down. Nil when nothing is
	// running.
	done chan struct{}
	// lastCfg + lastFeatureOn cache the server-side half of the
	// three-condition predicate so SetEnabled can re-evaluate
	// immediately when the local toggle flips, without waiting for
	// the next NewConfigEvent. Both are updated atomically when a
	// new config arrives.
	lastCfg       *C.UnboundedConfig
	lastFeatureOn bool

	// runningCfg is the snapshot of UnboundedConfig the live worker
	// was started with. broflake consumes its discovery/egress
	// options once in clientcore.NewBroflake, so a server-side config
	// change while the worker is alive would otherwise leave it
	// running on stale parameters. applyConfig compares this against
	// the freshly-cached lastCfg and triggers stop+start when they
	// differ, with the predicate still otherwise satisfied. Nil
	// whenever cancel is nil.
	runningCfg *C.UnboundedConfig
}

// shouldStart reports whether all three start conditions hold. Caller
// must hold m.mu.
func (m *unboundedManager) shouldStart() bool {
	return settings.GetBool(settings.UnboundedKey) && m.lastFeatureOn && m.lastCfg != nil
}

// cfgEqual reports whether two UnboundedConfig pointers refer to
// configurations broflake would consume identically. UnboundedConfig
// is a flat struct of strings and ints, so value equality is well-
// defined. Nil pointers compare equal to themselves and unequal to
// any non-nil pointer.
func cfgEqual(a, b *C.UnboundedConfig) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// Enabled reports whether the local opt-in is set. Doesn't say whether
// the proxy is currently running (server flag and config can override).
func Enabled() bool {
	return settings.GetBool(settings.UnboundedKey)
}

// SetEnabled persists the local opt-in (if it differs from the current
// persisted value) and re-evaluates the manager. Use this from direct
// callers (FFI, programmatic use) where the new toggle value hasn't
// been written to settings yet.
//
// PatchSettings persists settings itself before calling into the
// unbounded package, so it should use Apply() directly instead of
// going through SetEnabled — otherwise SetEnabled's no-change short-
// circuit (Enabled() == enable) returns before Apply runs and the
// manager never re-evaluates.
func SetEnabled(enable bool) error {
	if Enabled() != enable {
		if err := settings.Set(settings.UnboundedKey, enable); err != nil {
			return err
		}
		slog.Info("Unbounded widget proxy local opt-in changed", "enabled", enable)
	}
	return Apply()
}

// Apply re-evaluates the three-condition predicate (local toggle +
// server feature flag + server config cached) against the currently
// persisted setting and starts or stops the manager accordingly. Used
// by PatchSettings (which already persisted UnboundedKey itself) and
// by SetEnabled (after its persist step). Safe to call when nothing
// has changed — start is a no-op if the worker is already running and
// stop is a no-op if it isn't.
//
// No-op once Stop has disarmed the manager (post-shutdown): the
// armed gate is also checked inside start, so even a queued
// transition stays a no-op after Stop.
func Apply() error {
	if !Enabled() {
		manager.stop()
		return nil
	}
	manager.mu.Lock()
	armed := manager.armed
	shouldStart := manager.shouldStart()
	cfg := manager.lastCfg
	feature := manager.lastFeatureOn
	running := manager.cancel != nil
	manager.mu.Unlock()
	if !armed {
		return nil
	}
	if shouldStart {
		if !running {
			manager.start(cfg)
		}
		return nil
	}
	switch {
	case cfg == nil:
		slog.Info("Unbounded: enabled locally, waiting for server config")
	case !feature:
		slog.Info("Unbounded: enabled locally, but server feature flag is off")
	}
	return nil
}

// InitSubscription wires the manager into radiance's config event bus
// and applies any already-cached config. Called once at LocalBackend
// startup; the underlying subscription lives for the process lifetime
// (sync.Once-guarded), but the armed flag is set on every call so a
// Start-after-Close re-enables the manager that public Stop had
// disarmed.
//
// initial is the config that ConfigHandler has already loaded by the
// time Start reaches this line — typically the previously-persisted
// config from disk. Without seeding the manager state from it, the
// three-condition predicate stays stuck at lastCfg=nil/lastFeatureOn=
// false until the next config refresh arrives, and an already-opted-in
// user wouldn't auto-start the widget proxy until then. Pass nil if
// no config is available yet.
func InitSubscription(initial *config.Config) {
	initOnce.Do(func() {
		events.Subscribe(func(evt config.NewConfigEvent) {
			if evt.New == nil {
				return
			}
			applyConfig(*evt.New)
		})
	})
	manager.mu.Lock()
	manager.armed = true
	manager.mu.Unlock()
	if initial != nil {
		applyConfig(*initial)
	}
}

// applyConfig caches the server-side half of the start predicate and
// transitions the manager start/stop accordingly. Shared by
// InitSubscription's NewConfigEvent handler and the initial-config
// seeding path so cached and live configs follow identical logic.
// No-op when the manager is disarmed (post-Stop) so a late event
// arriving after backend shutdown doesn't revive the widget.
func applyConfig(cfg config.Config) {
	manager.mu.Lock()
	if !manager.armed {
		manager.mu.Unlock()
		return
	}
	// config.Config is a type alias for C.ConfigResponse on the
	// current radiance branch — no nested .ConfigResponse field,
	// just dereference and use directly.
	manager.lastCfg = cfg.Unbounded
	manager.lastFeatureOn = cfg.Features[C.UNBOUNDED]
	shouldRun := manager.shouldStart()
	running := manager.cancel != nil
	ucfg := manager.lastCfg
	cfgChanged := running && !cfgEqual(manager.runningCfg, ucfg)
	manager.mu.Unlock()

	switch {
	case shouldRun && !running:
		manager.start(ucfg)
	case shouldRun && cfgChanged:
		// Broflake consumed its options at construction time and has
		// no live-reconfigure API; the only way to pick up new
		// discovery/egress endpoints or table sizes is to tear the
		// worker down and bring it back up with the new config.
		// stop blocks until the prior worker fully exits, so start
		// always sees a clean slate.
		manager.stop()
		manager.start(ucfg)
	case !shouldRun && running:
		manager.stop()
	}
}

var initOnce sync.Once

// Stop tears down a running widget proxy and waits for the worker
// goroutine to actually exit (or the supplied ctx to expire). Used
// as a LocalBackend shutdown hook — without the wait, Close would
// return as soon as the cancel signal was queued and the broflake
// goroutine could still be inside NewBroflake or ui.Stop when the
// rest of the process tears down.
//
// Stop also disarms the manager: any subsequent start path (Apply,
// applyConfig from a config event, manager.start directly) becomes
// a no-op until InitSubscription re-arms. The config subscription
// callback stays installed but short-circuits via the armed gate,
// so a late config event arriving during or after Stop can't
// revive the widget. Future Start (after Close) re-arms via
// InitSubscription.
//
// Idempotent: no-op if no worker is running. Returns ctx.Err() if
// the wait deadline expires before the worker exits — in that case
// the worker has been signalled to cancel and will exit on its own
// schedule, but the caller has given up waiting.
func Stop(ctx context.Context) error {
	manager.transitionMu.Lock()
	defer manager.transitionMu.Unlock()
	manager.mu.Lock()
	manager.armed = false
	cancel := manager.cancel
	done := manager.done
	manager.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *unboundedManager) start(ucfg *C.UnboundedConfig) {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()

	m.mu.Lock()
	if !m.armed {
		// Disarmed by public Stop. Re-check inside transitionMu so a
		// start that got queued at transitionMu while Stop was waiting
		// for the worker still bails out instead of reviving the widget
		// after Stop's caller has returned.
		m.mu.Unlock()
		return
	}
	if m.cancel != nil {
		m.mu.Unlock()
		return // already running; transitionMu prevents overlap with stop
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	// Snapshot the config the worker is being started with so a
	// later applyConfig can detect parameter changes and restart.
	// Pointer-stored (not value-stored) because the upstream
	// lastCfg is also a pointer and equality is value-based via
	// cfgEqual.
	m.runningCfg = ucfg
	m.mu.Unlock()

	go func() {
		defer close(done)
		slog.Info("Unbounded: starting broflake widget proxy")

		bfOpt := clientcore.NewDefaultBroflakeOptions()
		bfOpt.ClientType = "widget"
		if ucfg != nil {
			if ucfg.CTableSize > 0 {
				bfOpt.CTableSize = ucfg.CTableSize
			}
			if ucfg.PTableSize > 0 {
				bfOpt.PTableSize = ucfg.PTableSize
			}
		}

		// Wire the broflake connection callback into the radiance event
		// bus so the Flutter globe (and any future abuse aggregation)
		// sees consumer connect/disconnect.
		bfOpt.OnConnectionChangeFunc = func(state int, workerIdx int, addr net.IP) {
			addrStr := ""
			if addr != nil {
				addrStr = addr.String()
			}
			slog.Debug("Unbounded: consumer connection change",
				"state", state, "workerIdx", workerIdx, "source", addrStr)
			events.Emit(ConnectionEvent{
				State:     state,
				Source:    addrStr,
				Timestamp: time.Now().UnixMilli(),
			})
		}

		rtcOpt := clientcore.NewDefaultWebRTCOptions()
		if ucfg != nil {
			if ucfg.DiscoverySrv != "" {
				rtcOpt.DiscoverySrv = ucfg.DiscoverySrv
			}
			if ucfg.DiscoveryEndpoint != "" {
				rtcOpt.Endpoint = ucfg.DiscoveryEndpoint
			}
		}

		egOpt := clientcore.NewDefaultEgressOptions()
		if ucfg != nil {
			if ucfg.EgressAddr != "" {
				egOpt.Addr = ucfg.EgressAddr
			}
			if ucfg.EgressEndpoint != "" {
				egOpt.Endpoint = ucfg.EgressEndpoint
			}
		}

		ui, err := newWidget(bfOpt, rtcOpt, egOpt)
		if err != nil {
			slog.Error("Unbounded: failed to create broflake widget", "error", err)
			cancel()
			m.mu.Lock()
			m.cancel = nil
			m.done = nil
			m.runningCfg = nil
			m.mu.Unlock()
			return
		}

		slog.Info("Unbounded: broflake widget proxy started")
		<-ctx.Done()
		slog.Info("Unbounded: stopping broflake widget proxy")
		ui.Stop()
		m.mu.Lock()
		m.cancel = nil
		m.done = nil
		m.runningCfg = nil
		m.mu.Unlock()
		slog.Info("Unbounded: broflake widget proxy stopped")
	}()
}

// stop signals the worker to exit and blocks until it does. Held
// under transitionMu so the worker fully unwinds (ui.Stop completes,
// m.cancel/m.done are cleared) before any other transition can
// observe state.
func (m *unboundedManager) stop() {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}
