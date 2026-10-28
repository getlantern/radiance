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
	"maps"
	"math/rand"
	"net"
	"slices"
	"sync"
	"time"

	C "github.com/getlantern/common"

	"github.com/getlantern/broflake/clientcore"
	"github.com/getlantern/broflake/common/helperticket"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
)

// ConnectionEvent fires every time a consumer (i.e. a censored client
// being routed through this widget proxy) connects or disconnects via
// the broflake mesh.
//
//	State     +1 on accept, -1 on close
//	Source    consumer's IP if broflake exposes it, otherwise empty
//	Timestamp emit time in Unix milliseconds
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

// connSources tracks the source addr of each live consumer slot so a close
// event — which broflake delivers with a nil addr — can be re-tagged with the
// addr its accept carried. Keyed by broflake's workerIdx (the consumer slot),
// stable across a single connection's accept→close. Without this, a close
// carries an empty Source, downstream consumers (the Flutter globe + helped
// counter) can't match it to the accept, and the connection's arc/count leaks.
// Concurrency-safe: broflake fires connection-change callbacks from per-worker
// goroutines.
type connSources struct {
	mu    sync.Mutex
	addrs map[int]string
}

func newConnSources() *connSources {
	return &connSources{addrs: make(map[int]string)}
}

// resolve records the addr on accept (state > 0) or restores it on close
// (state < 0, where broflake's addr is nil), returning the Source the
// ConnectionEvent should carry. An accept with an empty addr is left
// untracked, so its close stays empty too — neither is counted, which is the
// right behavior when broflake can't surface the consumer IP at all.
func (c *connSources) resolve(state, workerIdx int, addrStr string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case state > 0:
		if addrStr != "" {
			c.addrs[workerIdx] = addrStr
		}
	case state < 0:
		if addrStr == "" {
			addrStr = c.addrs[workerIdx]
		}
		delete(c.addrs, workerIdx)
	}
	return addrStr
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

	// tickets supplies the volunteer tickets for this device's helper
	// pairings. nil: the widget joins no rooms.
	tickets roomTickets

	// runningCfg is the snapshot of UnboundedConfig the live worker
	// was started with. broflake consumes its discovery/egress
	// options once in clientcore.NewBroflake, so a server-side config
	// change while the worker is alive would otherwise leave it
	// running on stale parameters. applyConfig compares this against
	// the freshly-cached lastCfg and triggers stop+start when they
	// differ, with the predicate still otherwise satisfied. Nil
	// whenever cancel is nil.
	runningCfg *C.UnboundedConfig
	running    bool
	peers      map[int]string
	arrivals   uint64

	// slots receives per-chunk byte counts from the live worker; it is
	// folded into bytesDown/bytesUp and peerBytes (keyed by consumer IP)
	// whenever a slot's connection changes or a snapshot is taken.
	slots     *slotTable
	bytesDown uint64
	bytesUp   uint64
	peerBytes map[string]*peerTally
	// lastOwner keeps a closed slot's tally so bytes broflake reports after the
	// close are still credited to it.
	lastOwner map[int]*peerTally
	// retired holds stopped runs' tables still absorbing late bytes.
	retired []retiredRun
	// now overrides time.Now for peer retention tests.
	now func() time.Time
}

// shouldStart reports whether all three start conditions hold. Caller
// must hold m.mu.
func (m *unboundedManager) shouldStart() bool {
	return settings.GetBool(settings.UnboundedKey) && m.lastFeatureOn && cfgUsable(m.lastCfg)
}

// cfgUsable reports whether the cached UnboundedConfig supplies the
// minimum fields broflake needs to route real consumer traffic:
// discovery (server + endpoint) and egress (address + endpoint). The
// server contract is that all four are required; broflake's
// clientcore defaults exist for unit-test convenience and point at
// the upstream maintainer's infra — running them in a Lantern build
// would bypass the server's per-environment endpoint selection and
// the "is this user opted in?" feature-flag gate, so a partially-
// populated config is treated as "not yet ready to start" rather
// than "fall back to defaults".
//
// CTableSize / PTableSize are not required; defaults are reasonable
// and the server sends them only when it wants to override.
func cfgUsable(cfg *C.UnboundedConfig) bool {
	if cfg == nil {
		return false
	}
	return cfg.DiscoverySrv != "" && cfg.DiscoveryEndpoint != "" &&
		cfg.EgressAddr != "" && cfg.EgressEndpoint != ""
}

func donorSTUNBatch(servers []string) func(uint32) ([]string, error) {
	pool := C.NormalizeDonorSTUNServers(servers)
	return func(size uint32) ([]string, error) {
		batch := slices.Clone(pool)
		rand.Shuffle(len(batch), func(i, j int) { batch[i], batch[j] = batch[j], batch[i] })
		if uint64(size) < uint64(len(batch)) {
			batch = batch[:size]
		}
		return batch, nil
	}
}

// roomTickets is where the widget gets its helper pairing tickets;
// helper.Source in production.
type roomTickets interface {
	Volunteer() []string
	AnsweredVolunteer() []string
}

// SetTicketSource sets where the widget gets its volunteer tickets. A widget
// already running keeps its source until it restarts.
func SetTicketSource(src roomTickets) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.tickets = src
}

// roomSyncInterval is how often a running widget re-reads its tickets and the
// serve-pool setting. The source is a cache that renews tickets every 10
// minutes, so this costs nothing. A variable so tests can shorten it.
var roomSyncInterval = 30 * time.Second

// servePool is the "help others too" setting, shared with direct mode: unset
// means on.
func servePool() bool {
	return settings.Get(settings.PeerServePoolKey) == nil || settings.GetBool(settings.PeerServePoolKey)
}

// applyRooms points a new widget's options at the volunteer tickets and sets
// the serve-pool setting. broflake calls the source at every genesis message,
// so fresh tickets reach the widget as soon as the source has them; the source
// is a cache whose reads never block. The returned set records each pairing
// whose tickets reach signaling, for syncRooms. Call it before NewBroflake.
func applyRooms(rtcOpt *clientcore.WebRTCOptions, tickets roomTickets) *servedPairings {
	served := &servedPairings{ids: make(map[string]bool)}
	if tickets != nil {
		rtcOpt.RoomTicketSource = func() []string {
			tks := tickets.Volunteer()
			served.add(tks)
			return tks
		}
	}
	rtcOpt.SetServePool(servePool())
	return served
}

// servedPairings is the set of pairing IDs whose tickets a widget has handed
// to signaling. Recording them as they go out, from the widget's first
// genesis, means a pairing that ends before syncRooms first looks still ends
// its sessions. Like volunteerPairings, it is never logged.
type servedPairings struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (s *servedPairings) add(tickets []string) {
	ids := volunteerPairings(tickets)
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range ids {
		s.ids[id] = true
	}
}

// endedIn reports whether a served pairing is missing from the tickets answered
// returns. It copies the served set before calling answered: a pairing served
// by then was in an earlier answer, so missing from this one it has ended,
// while one served only afterwards may come from a newer answer than this.
func (s *servedPairings) endedIn(answered func() []string) bool {
	s.mu.Lock()
	served := make([]string, 0, len(s.ids))
	for id := range s.ids {
		served = append(served, id)
	}
	s.mu.Unlock()
	cur := volunteerPairings(answered())
	for _, id := range served {
		if !cur[id] {
			return true
		}
	}
	return false
}

// volunteerPairings is the set of pairing IDs tickets claim. It is unverified
// and is used only to notice a pairing disappearing; it is never logged.
func volunteerPairings(tickets []string) map[string]bool {
	out := make(map[string]bool, len(tickets))
	for _, tk := range tickets {
		if _, id := helperticket.Peek(tk); id != "" {
			out[id] = true
		}
	}
	return out
}

// syncRooms keeps a running widget's pool setting current and watches for
// changes that must end its open sessions, until ctx ends. done identifies the
// run. Two changes restart the widget:
//
//   - a pairing it has served is missing from the tickets lantern-cloud last
//     answered with: the pairing has ended. freddie stops new matches by
//     itself, but a WebRTC session already open outlives the tickets, and the
//     widget cannot tell which session came through which room. Tickets that
//     only expired, say while lantern-cloud is unreachable, end nothing;
//   - "help others too" turned off: sessions already open with strangers
//     must end too.
func (m *unboundedManager) syncRooms(ctx context.Context, done chan struct{}, rtcOpt *clientcore.WebRTCOptions, tickets roomTickets, served *servedPairings, interval time.Duration) {
	prevPool := rtcOpt.ServesPool()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pool := servePool()
		rtcOpt.SetServePool(pool)
		reason := ""
		if prevPool && !pool {
			reason = "Unbounded: helping others turned off; restarting the widget to close its sessions"
		}
		if tickets != nil && served.endedIn(tickets.AnsweredVolunteer) {
			reason = "Unbounded: a helper pairing ended; restarting the widget to close its sessions"
		}
		if reason != "" {
			slog.Info(reason)
			go m.restartIfCurrent(done)
			return
		}
		prevPool = pool
	}
}

// restartIfCurrent stops and restarts the widget if the run done identifies
// is still the live one. The check and the restart hold transitionMu
// together, so a run that something else already stopped or replaced is
// never bounced.
func (m *unboundedManager) restartIfCurrent(done chan struct{}) {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.mu.Lock()
	current := done != nil && m.done == done
	m.mu.Unlock()
	if !current {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), internalStopTimeout)
	defer cancel()
	if err := m.stopLocked(ctx, false); err != nil {
		slog.Warn("Unbounded: internal stop timed out before worker exited",
			"error", err, "timeout", internalStopTimeout)
		// The old worker still holds the run, so startLocked would do
		// nothing. Start once it exits; start re-checks that the manager is
		// armed and nothing else started, so a public Stop meanwhile wins.
		go func() {
			<-done
			m.start()
		}()
		return
	}
	m.startLocked()
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
	if h := applyHook; h != nil {
		h()
	}
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
			manager.start()
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

// applyConfig ignores updates after Stop so late events cannot restart the widget.
func applyConfig(cfg config.Config) {
	manager.mu.Lock()
	if !manager.armed {
		manager.mu.Unlock()
		return
	}
	manager.lastCfg = cfg.Unbounded
	if cfg.Unbounded != nil {
		copied := *cfg.Unbounded
		copied.STUNServers = slices.Clone(cfg.Unbounded.STUNServers)
		copied.Tuning = maps.Clone(cfg.Unbounded.Tuning)
		manager.lastCfg = &copied
	}
	manager.lastFeatureOn = cfg.Features[C.UNBOUNDED]
	shouldRun := manager.shouldStart()
	running := manager.cancel != nil
	ucfg := manager.lastCfg
	cfgChanged := running && !manager.runningCfg.Equal(ucfg)
	manager.mu.Unlock()

	switch {
	case shouldRun && !running:
		manager.start()
	case shouldRun && cfgChanged:
		// Broflake consumed its options at construction time and has
		// no live-reconfigure API; the only way to pick up new
		// discovery/egress endpoints or table sizes is to tear the
		// worker down and bring it back up with the new config.
		// stop blocks until the prior worker fully exits, so start
		// always sees a clean slate.
		manager.stop()
		manager.start()
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
// schedule, but the caller has given up waiting. m.cancel and
// m.done stay set until the worker eventually clears them, so a
// subsequent start observes "already running" and is a no-op.
func Stop(ctx context.Context) error {
	return manager.stopCtx(ctx, true)
}

// internalStopTimeout bounds how long Apply / applyConfig wait for
// the worker to exit after signalling cancel. broflake's ui.Stop
// should drain in well under this; a longer-than-expected ui.Stop
// must not block a settings PATCH or a config-event handler
// indefinitely. Variable (not const) so tests can install a short
// timeout to exercise the timeout path without holding up the
// suite. Production code reads this on every call, so the override
// applies for the duration of the test.
var internalStopTimeout = 5 * time.Second

// applyHook is invoked at the top of Apply, before any state check
// or transition. nil in production; backend tests install a counter
// or assertion to verify that PatchSettings actually dispatches the
// UnboundedKey diff to this package. Keep this minimal — exposing
// the manager's internals across packages just for test wiring
// would be a much bigger surface.
var applyHook func()

// SetApplyHookForTest installs h to be invoked at the start of
// every Apply call. Pass nil to remove. Test-only; production code
// must not call this. The hook fires regardless of the Enabled()
// gate so callers can verify dispatch happened even when no
// transition results.
func SetApplyHookForTest(h func()) {
	applyHook = h
}

// stopCtx is the shared implementation for both the public Stop
// (disarm=true) and internal manager.stop (disarm=false). Holds
// transitionMu for the entire signal+wait so a concurrent start
// cannot interleave. On ctx expiration the cancel signal has been
// delivered and the worker will exit on its own schedule; manager
// state is left intact so future transitions see "already running"
// until the worker clears it.
func (m *unboundedManager) stopCtx(ctx context.Context, disarm bool) error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	return m.stopLocked(ctx, disarm)
}

// stopLocked is stopCtx for a caller that holds transitionMu.
func (m *unboundedManager) stopLocked(ctx context.Context, disarm bool) error {
	m.mu.Lock()
	if disarm {
		m.armed = false
	}
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	m.mu.Lock()
	m.running = false
	m.releaseAllPeers()
	m.peers = nil
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// start brings up the broflake worker if all preconditions hold at
// the moment it acquires the locks: manager armed, no worker already
// running, and the three-condition predicate (toggle + feature flag +
// cached config) still satisfied. Every check is done INSIDE
// transitionMu so a caller queued behind a stop or another start
// observes the freshest state rather than a snapshot from when it
// decided to start — a concurrent SetEnabled(false) or config update
// between the caller's predicate read and start's lock acquisition
// is honored.
//
// The config used by the worker is the LIVE m.lastCfg, not a snapshot
// captured by the caller. applyConfig updates m.lastCfg before
// calling start, so this gives identical behavior for the normal
// path; for the race case (config updated after caller decided to
// start), the worker comes up with the latest parameters.
func (m *unboundedManager) start() {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.startLocked()
}

// startLocked is start for a caller that holds transitionMu.
func (m *unboundedManager) startLocked() {
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
	if !m.shouldStart() {
		// Predicate flipped while we were queued at transitionMu — a
		// SetEnabled(false), a config event that cleared the feature
		// flag or unset the cfg, or any other concurrent change. Bail
		// rather than start a worker that's already been decided
		// against.
		m.mu.Unlock()
		return
	}
	ucfg := m.lastCfg
	tickets := m.tickets
	syncInterval := roomSyncInterval
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	// Retain the immutable config snapshot to detect changes while this worker runs.
	m.runningCfg = ucfg
	m.peers = make(map[int]string)
	slots := &slotTable{}
	m.slots = slots
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
		//
		// Cancellation drain: broflake's per-worker connection-change
		// goroutines can fire callbacks concurrently with ui.Stop, and
		// the broflake API doesn't promise no-callbacks-after-Stop.
		// Check ctx.Err() at the top so callbacks delivered after stop
		// signals cancel — but before broflake's internal teardown
		// drained — short-circuit instead of pushing a stale connection
		// event onto the bus after the consumer thinks Unbounded is
		// off. broflake exposes no registration point we could
		// disarm directly (the callback IS the registration), so the
		// inline ctx check is the next-best place.
		// Close callbacks omit the address; preserve it for connection-event subscribers.
		sources := newConnSources()
		bfOpt.OnConnectionChangeFunc = func(state int, workerIdx int, addr net.IP) {
			if ctx.Err() != nil {
				return
			}
			addrStr := ""
			if addr != nil {
				addrStr = addr.String()
			}
			addrStr = sources.resolve(state, workerIdx, addrStr)
			m.recordConnection(ctx, state, workerIdx, addrStr)
			slog.Debug("Unbounded: consumer connection change",
				"state", state, "workerIdx", workerIdx, "source", addrStr)
			events.Emit(ConnectionEvent{
				State:     state,
				Source:    addrStr,
				Timestamp: time.Now().UnixMilli(),
			})
		}

		// Downstream chunks fire per chunk on broflake's data path, so the
		// counters stay lock-free and attribution to an IP happens later
		// under m.mu. Chunks arriving after stop land in the retired run,
		// credited to the slot owners at stop.
		bfOpt.OnDownstreamChunkFunc = chunkCounter(slots, true)
		bfOpt.OnUpstreamChunkFunc = chunkCounter(slots, false)

		rtcOpt := clientcore.NewDefaultWebRTCOptions()
		// A genesis POST carries this widget's room tickets; never let a
		// redirect replay it to another origin.
		rtcOpt.HTTPClient = sameOriginRedirects(rtcOpt.HTTPClient)
		rtcOpt.STUNBatch = donorSTUNBatch(nil)
		if ucfg != nil {
			rtcOpt.STUNBatch = donorSTUNBatch(ucfg.STUNServers)
			if ucfg.DiscoverySrv != "" {
				rtcOpt.DiscoverySrv = ucfg.DiscoverySrv
			}
			if ucfg.DiscoveryEndpoint != "" {
				rtcOpt.Endpoint = ucfg.DiscoveryEndpoint
			}
			var ignored []string
			rtcOpt.Tuning, ignored = clientcore.ParseWebRTCTuning(ucfg.Tuning)
			if len(ignored) > 0 {
				slog.Warn("Unbounded: ignoring widget tuning", "ignored", ignored)
			}
		}
		// Helper pairing rooms: freddie offers this widget's genesis messages
		// to its paired consumers first, and with "help others too" off to
		// nobody else.
		served := applyRooms(rtcOpt, tickets)

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

		m.mu.Lock()
		m.running = ctx.Err() == nil
		m.mu.Unlock()
		slog.Info("Unbounded: broflake widget proxy started")
		// The worker waits for syncRooms before reporting done, so a stopped
		// run leaves nothing behind.
		syncDone := make(chan struct{})
		go func() {
			defer close(syncDone)
			m.syncRooms(ctx, done, rtcOpt, tickets, served, syncInterval)
		}()
		<-ctx.Done()
		slog.Info("Unbounded: stopping broflake widget proxy")
		ui.Stop()
		<-syncDone
		m.mu.Lock()
		m.cancel = nil
		m.done = nil
		m.runningCfg = nil
		m.mu.Unlock()
		slog.Info("Unbounded: broflake widget proxy stopped")
	}()
}

// stop is the internal (no-arg) variant called by Apply and
// applyConfig. It wraps stopCtx with a default timeout so a hung
// ui.Stop doesn't block settings PATCHes or config-event handling
// indefinitely. The timeout error is logged because the call site
// has no useful action to take; subsequent transitions observe
// "already running" until the worker eventually exits.
func (m *unboundedManager) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), internalStopTimeout)
	defer cancel()
	if err := m.stopCtx(ctx, false); err != nil {
		slog.Warn("Unbounded: internal stop timed out before worker exited",
			"error", err, "timeout", internalStopTimeout)
	}
}
