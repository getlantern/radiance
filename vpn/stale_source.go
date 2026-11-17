package vpn

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/x/list"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// A device can come back from a network change with its new sockets still
// sourced from an address the interface no longer holds, which a sing-box
// network reset doesn't clear. The checker looks for that after each default
// interface update, from this process, and escalates from a reset to a tunnel
// restart.

// Vars so tests can shorten them.
var (
	staleSourceSchedule = []time.Duration{
		2 * time.Second, 5 * time.Second, 10 * time.Second,
		20 * time.Second, 40 * time.Second, 60 * time.Second,
	}
	staleSourceRecheckDelay = 3 * time.Second
)

const (
	// staleSourceRestartInterval is the least time between restarts for a
	// stale source, across the client's tunnels.
	staleSourceRestartInterval = 10 * time.Minute
	// staleSourceRestartWindow bounds how long after a restart the next
	// conclusive check is taken as its outcome.
	staleSourceRestartWindow = 2 * time.Minute
	// staleSourceUnknownRechecks bounds the rechecks after a reset that
	// come back inconclusive, so the episode still ends with an outcome.
	staleSourceUnknownRechecks = 3
)

type sourceStatus int

const (
	sourceUnknown sourceStatus = iota
	sourceHealthy
	sourceStale
)

func (s sourceStatus) String() string {
	switch s {
	case sourceHealthy:
		return "healthy"
	case sourceStale:
		return "stale"
	default:
		return "unknown"
	}
}

// sourceCheckResult is one check of the default interface's source address.
// Addresses appear only as last octets.
type sourceCheckResult struct {
	status sourceStatus
	iface  string
	index  int
	// addrs are the interface's current IPv4 addresses.
	addrs []string
	// source is the source the kernel chose, or the error class.
	source string
	// bound is the result of a socket bound to the first current address,
	// tried only when stale.
	bound   string
	boundOK bool
}

// sourceCheckFunc checks the source address the kernel picks for a new
// socket sing-box would open on iface.
type sourceCheckFunc func(iface *control.Interface) sourceCheckResult

type recoveryStep int

const (
	stepNone recoveryStep = iota
	stepReset
	stepRestart
	stepGaveUp
)

func (s recoveryStep) String() string {
	switch s {
	case stepReset:
		return "reset"
	case stepRestart:
		return "restart"
	case stepGaveUp:
		return "gave_up"
	default:
		return "none"
	}
}

// Outcomes of a stale source episode, for the vpn.stale_source counter.
const (
	staleOutcomeRecoveredByReset   = "recovered_by_reset"
	staleOutcomeRecoveredByRestart = "recovered_by_restart"
	staleOutcomeNotRecovered       = "not_recovered"
	staleOutcomeRestartRateLimited = "restart_rate_limited"
)

// staleSourceGuard is the restart state a VPNClient's tunnels share, so the
// rate limit and a restart's outcome survive the restart.
type staleSourceGuard struct {
	mu          sync.Mutex
	lastRestart time.Time
	pending     bool
	// pendingBoundOK is the bound socket result the restart was taken on.
	pendingBoundOK bool
}

// claimRestart reports whether a restart may run at now, and if so records
// it as pending. prev is the restart it replaces, for rollback.
func (g *staleSourceGuard) claimRestart(now time.Time, boundOK bool) (prev time.Time, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.lastRestart.IsZero() && now.Sub(g.lastRestart) < staleSourceRestartInterval {
		return time.Time{}, false
	}
	prev = g.lastRestart
	g.lastRestart = now
	g.pending = true
	g.pendingBoundOK = boundOK
	return prev, true
}

// rollback undoes a claimed restart that never ran, so it neither holds
// the rate limit nor awaits an outcome. It reports whether the restart was
// still pending.
func (g *staleSourceGuard) rollback(prev time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.pending {
		return false
	}
	g.pending = false
	g.lastRestart = prev
	return true
}

// takePending reports whether a restart is awaiting its outcome, clearing
// it. A restart older than staleSourceRestartWindow has none.
func (g *staleSourceGuard) takePending(now time.Time) (pending, boundOK bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pending = g.pending && now.Sub(g.lastRestart) < staleSourceRestartWindow
	g.pending = false
	return pending, g.pendingBoundOK
}

// staleSourceMachine runs the check schedule and recovery for one tunnel. It
// is driven by update and tick from one goroutine, with now passed in.
type staleSourceMachine struct {
	schedule     []time.Duration
	recheckDelay time.Duration

	defaultIface func() *control.Interface
	check        sourceCheckFunc
	reset        func()
	// restart starts a tunnel restart without waiting for it, calling
	// failed, from another goroutine, if it doesn't happen; nil when the
	// client has no restart path.
	restart func(failed func())
	guard   *staleSourceGuard
	record  func(outcome string, boundOK bool)
	logger  *slog.Logger

	active    bool
	updateAt  time.Time
	next      int
	recheckAt time.Time
	stale     int
	step      recoveryStep
	boundOK   bool
	// unknownRechecks counts inconclusive checks since the reset.
	unknownRechecks int
}

// update starts the schedule over for a default interface update, or stops
// it while there is no default interface.
func (m *staleSourceMachine) update(now time.Time, hasIface bool) {
	m.active = hasIface
	m.updateAt = now
	m.next = 0
	m.recheckAt = time.Time{}
	m.stale = 0
	m.step = stepNone
	m.boundOK = false
	m.unknownRechecks = 0
}

// stop ends the schedule until the next update.
func (m *staleSourceMachine) stop() {
	m.active = false
	m.next = len(m.schedule)
	m.recheckAt = time.Time{}
}

// nextDue returns when the next check is due, if one is.
func (m *staleSourceMachine) nextDue() (time.Time, bool) {
	if !m.active {
		return time.Time{}, false
	}
	var due time.Time
	if m.next < len(m.schedule) {
		due = m.updateAt.Add(m.schedule[m.next])
	}
	if !m.recheckAt.IsZero() && (due.IsZero() || m.recheckAt.Before(due)) {
		due = m.recheckAt
	}
	return due, !due.IsZero()
}

// tick runs the check that is due at now.
func (m *staleSourceMachine) tick(now time.Time) {
	if !m.active {
		return
	}
	if !m.recheckAt.IsZero() && !now.Before(m.recheckAt) {
		m.recheckAt = time.Time{}
	}
	for m.next < len(m.schedule) && !now.Before(m.updateAt.Add(m.schedule[m.next])) {
		m.next++
	}
	iface := m.defaultIface()
	if iface == nil {
		m.active = false
		return
	}
	m.conclude(now, m.check(iface))
	if _, ok := m.nextDue(); !ok {
		m.active = false
	}
}

func (m *staleSourceMachine) conclude(now time.Time, res sourceCheckResult) {
	if res.status == sourceUnknown {
		m.logger.Debug("Source address check inconclusive", m.attrs(now, res)...)
		if m.step != stepReset {
			return
		}
		if m.unknownRechecks < staleSourceUnknownRechecks {
			m.unknownRechecks++
			m.recheckAt = now.Add(m.recheckDelay)
			return
		}
		m.step = stepGaveUp
		m.logger.Warn("Stale source address recovery failed: checks inconclusive after network reset", m.attrs(now, res)...)
		m.record(staleOutcomeNotRecovered, m.boundOK)
		return
	}
	m.unknownRechecks = 0
	// A conclusive check after a restart is that restart's outcome. The
	// tunnel that asked for the restart stops checking, so this is the
	// restarted tunnel's.
	var restarted, restartBoundOK bool
	if m.step == stepNone {
		restarted, restartBoundOK = m.guard.takePending(now)
	}
	if res.status == sourceHealthy {
		switch {
		case restarted:
			m.logger.Info("Source address healthy after tunnel restart", m.attrs(now, res)...)
			m.record(staleOutcomeRecoveredByRestart, restartBoundOK)
		case m.step == stepReset:
			m.logger.Info("Source address healthy after network reset", m.attrs(now, res)...)
			m.record(staleOutcomeRecoveredByReset, m.boundOK)
		case m.stale > 0:
			m.logger.Info("Source address healthy again", m.attrs(now, res)...)
		}
		m.stale = 0
		m.step = stepNone
		m.recheckAt = time.Time{}
		return
	}

	m.boundOK = res.boundOK
	m.logger.Warn("Stale source address", m.attrs(now, res)...)
	if restarted {
		m.step = stepGaveUp
		m.logger.Warn("Stale source address recovery failed: still stale after tunnel restart", m.attrs(now, res)...)
		m.record(staleOutcomeNotRecovered, res.boundOK)
		return
	}
	m.stale++
	switch m.step {
	case stepNone:
		if m.stale < 2 {
			return
		}
		m.step = stepReset
		m.recheckAt = now.Add(m.recheckDelay)
		m.logger.Warn("Stale source address recovery: resetting network", m.attrs(now, res)...)
		m.reset()
	case stepReset:
		m.escalate(now, res)
	}
}

func (m *staleSourceMachine) escalate(now time.Time, res sourceCheckResult) {
	m.step = stepGaveUp
	if m.restart == nil {
		m.logger.Warn("Stale source address recovery failed: still stale after network reset, and no tunnel restart path", m.attrs(now, res)...)
		m.record(staleOutcomeNotRecovered, res.boundOK)
		return
	}
	prev, ok := m.guard.claimRestart(now, res.boundOK)
	if !ok {
		m.logger.Warn("Stale source address recovery failed: still stale after network reset, and a tunnel restart ran within the last 10 minutes", m.attrs(now, res)...)
		m.record(staleOutcomeRestartRateLimited, res.boundOK)
		return
	}
	m.step = stepRestart
	attrs := m.attrs(now, res)
	m.logger.Warn("Stale source address recovery: restarting tunnel", attrs...)
	guard, record, logger, boundOK := m.guard, m.record, m.logger, res.boundOK
	m.restart(func() {
		if guard.rollback(prev) {
			logger.Warn("Stale source address recovery failed: tunnel restart failed", attrs...)
			record(staleOutcomeNotRecovered, boundOK)
		}
	})
	// The restarted tunnel's checker takes the outcome. Left running, this
	// one could take it first, before the restart has happened.
	m.stop()
}

func (m *staleSourceMachine) attrs(now time.Time, res sourceCheckResult) []any {
	attrs := []any{
		"iface", res.iface,
		"iface_index", res.index,
		"addrs", strings.Join(res.addrs, ","),
		"source", res.source,
		"result", res.status.String(),
		"since_update_s", math.Round(now.Sub(m.updateAt).Seconds()*10) / 10,
		"step", m.step.String(),
	}
	if res.bound != "" {
		attrs = append(attrs, "bound", res.bound)
	}
	return attrs
}

var (
	staleSourceCounterOnce sync.Once
	staleSourceCounter     metric.Int64Counter
)

// staleSourceRecord records an episode's outcome; a var so tests can watch it.
var staleSourceRecord = recordStaleSourceOutcome

func recordStaleSourceOutcome(outcome string, boundOK bool) {
	staleSourceCounterOnce.Do(func() {
		c, err := otel.Meter("github.com/getlantern/radiance/metrics").Int64Counter(
			"vpn.stale_source",
			metric.WithDescription("Outcomes of stale source address episodes"),
		)
		if err != nil {
			slog.Warn("Failed to create stale source counter", "error", err)
			return
		}
		staleSourceCounter = c
	})
	if staleSourceCounter == nil {
		return
	}
	staleSourceCounter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.Bool("bound_ok", boundOK),
	))
}

// defaultInterfaceMonitor is the subset of the box's interface monitor the
// checker uses.
type defaultInterfaceMonitor interface {
	DefaultInterface() *control.Interface
	MyInterfaces() []string
	RegisterCallback(callback tun.DefaultInterfaceUpdateCallback) *list.Element[tun.DefaultInterfaceUpdateCallback]
	UnregisterCallback(element *list.Element[tun.DefaultInterfaceUpdateCallback])
}

// startStaleSourceChecker runs m off monitor's updates until the returned
// closer is closed. It starts a schedule at once when there already is a
// default interface, since the tunnel's own first update came before it.
// Calling update starts the schedule over as a monitor update does; it never
// blocks.
func startStaleSourceChecker(ctx context.Context, monitor defaultInterfaceMonitor, m *staleSourceMachine) (closer io.Closer, update func()) {
	updates := make(chan struct{}, 1)
	signal := func() {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
	// The callback runs on the monitor's update path, so it only signals.
	elem := monitor.RegisterCallback(func(_ *control.Interface, flags int) {
		if flags&tun.FlagAndroidVPNUpdate == 0 {
			signal()
		}
	})
	if monitor.DefaultInterface() != nil {
		signal()
	}
	run := runUntilClosed(ctx, func(ctx context.Context) {
		for {
			var (
				fire  <-chan time.Time
				timer *time.Timer
			)
			if due, ok := m.nextDue(); ok {
				timer = time.NewTimer(time.Until(due))
				fire = timer.C
			}
			select {
			case <-ctx.Done():
			case <-updates:
				m.update(time.Now(), monitor.DefaultInterface() != nil)
			case <-fire:
				m.tick(time.Now())
			}
			if timer != nil {
				timer.Stop()
			}
			if ctx.Err() != nil {
				return
			}
		}
	})
	return closerFunc(func() error {
		monitor.UnregisterCallback(elem)
		return run.Close()
	}), signal
}

func lastOctet(a netip.Addr) string {
	if !a.Is4() {
		return "non_ipv4"
	}
	return "." + strconv.Itoa(int(a.As4()[3]))
}

// startStaleSourceChecker starts the tunnel's stale source checker, or
// returns nil where there is none.
func (t *tunnel) startStaleSourceChecker() io.Closer {
	closer, update := t.newStaleSourceChecker()
	t.staleSourceUpdate = update
	return closer
}

// restartStaleSourceSchedule starts the stale source check schedule over, as
// a default interface update does. The platform resets the network when the
// default interface keeps its name and index but changes address, which the
// monitor doesn't report. No-op without a checker.
func (t *tunnel) restartStaleSourceSchedule() {
	if t.staleSourceUpdate != nil {
		t.staleSourceUpdate()
	}
}

func (t *tunnel) newStaleSourceChecker() (io.Closer, func()) {
	if newSourceCheck == nil || t.boxInstance == nil {
		return nil, nil
	}
	network := t.boxInstance.Network()
	monitor := network.InterfaceMonitor()
	if monitor == nil {
		return nil, nil
	}
	guard := t.staleSource
	if guard == nil {
		guard = &staleSourceGuard{}
	}
	ctx := t.ctx
	m := &staleSourceMachine{
		schedule:     staleSourceSchedule,
		recheckDelay: staleSourceRecheckDelay,
		defaultIface: monitor.DefaultInterface,
		check:        newSourceCheck(ctx, monitor),
		// What tunnel.resetNetwork does, without reading boxInstance, which
		// close clears off c.mu's hold. Not VPNClient.ResetNetwork, which
		// would start the schedule over.
		reset:   func() { network.ResetNetwork(ctx) },
		restart: t.restartForStaleSource,
		guard:   guard,
		record:  staleSourceRecord,
		logger:  slog.Default(),
	}
	return startStaleSourceChecker(ctx, monitor, m)
}

// staleSourceRestart returns how c's tunnels restart for a stale source
// address, or nil without a platform interface: the direct restart path
// rebuilds from BoxOptions only the backend has, while the platform's path
// reconnects through the backend.
func (c *VPNClient) staleSourceRestart() func(failed func()) {
	if c.platformIfce == nil {
		return nil
	}
	return func(failed func()) {
		// Detached: the restart closes the tunnel, and closing it waits for
		// the checker that calls this.
		go func() {
			if err := c.Restart(BoxOptions{}); err != nil {
				c.logger.Warn("Tunnel restart for stale source address failed", "error", err)
				failed()
			}
		}()
	}
}
