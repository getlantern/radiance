package vpn

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/x/list"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staleOutcome struct {
	outcome string
	boundOK bool
}

type staleHarness struct {
	m        *staleSourceMachine
	results  []sourceStatus
	checks   []time.Duration
	resets   int
	restarts int
	outcomes []staleOutcome
	t0       time.Time
	logs     *bytes.Buffer
}

// newStaleHarness returns a machine whose checks report results in turn,
// then healthy.
func newStaleHarness(guard *staleSourceGuard, results ...sourceStatus) *staleHarness {
	h := &staleHarness{results: results, t0: time.Unix(1_000_000, 0), logs: &bytes.Buffer{}}
	iface := &control.Interface{Name: "en0", Index: 4}
	h.m = &staleSourceMachine{
		schedule:     staleSourceSchedule,
		recheckDelay: staleSourceRecheckDelay,
		defaultIface: func() *control.Interface { return iface },
		check: func(iface *control.Interface) sourceCheckResult {
			status := sourceHealthy
			if len(h.results) > 0 {
				status, h.results = h.results[0], h.results[1:]
			}
			res := sourceCheckResult{status: status, iface: iface.Name, index: iface.Index, addrs: []string{".20"}, source: ".20"}
			if status == sourceStale {
				res.source, res.bound, res.boundOK = ".22", "ok", true
			}
			return res
		},
		reset:   func() { h.resets++ },
		restart: func() { h.restarts++ },
		guard:   guard,
		record:  func(o string, b bool) { h.outcomes = append(h.outcomes, staleOutcome{o, b}) },
		logger:  slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return h
}

// runChecks runs up to n due checks, recording when each ran.
func (h *staleHarness) runChecks(n int) {
	for range n {
		due, ok := h.m.nextDue()
		if !ok {
			return
		}
		h.checks = append(h.checks, due.Sub(h.t0))
		h.m.tick(due)
	}
}

func secs(s ...float64) []time.Duration {
	var ds []time.Duration
	for _, v := range s {
		ds = append(ds, time.Duration(v*float64(time.Second)))
	}
	return ds
}

func TestStaleSourceSchedule(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{})
	_, ok := h.m.nextDue()
	require.False(t, ok, "nothing runs before an update")
	h.m.update(h.t0, true)
	h.runChecks(100)
	assert.Equal(t, secs(2, 5, 10, 20, 40, 60), h.checks)
	_, ok = h.m.nextDue()
	assert.False(t, ok, "quiet after the schedule")
	assert.Empty(t, h.outcomes)
}

func TestStaleSourceUpdateRestartsSchedule(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{}, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(2)
	h.m.update(h.t0.Add(7*time.Second), true)
	h.runChecks(1)
	assert.Equal(t, secs(2, 5, 9), h.checks)
	assert.Zero(t, h.resets, "a new update starts the stale count over")
}

func TestStaleSourceNoDefaultInterface(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{})
	h.m.update(h.t0, false)
	_, ok := h.m.nextDue()
	assert.False(t, ok)

	h.m.update(h.t0, true)
	h.m.defaultIface = func() *control.Interface { return nil }
	h.runChecks(100)
	assert.Equal(t, secs(2), h.checks, "the schedule stops once the default interface is gone")
}

func TestStaleSourceOneStaleDoesNotRecover(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{}, sourceStale, sourceHealthy, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(100)
	assert.Zero(t, h.resets)
	assert.Zero(t, h.restarts)
	assert.Empty(t, h.outcomes)
}

func TestStaleSourceUnknownIsIgnored(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{}, sourceUnknown, sourceStale, sourceUnknown, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(4)
	assert.Equal(t, 1, h.resets, "unknown checks neither count nor break a run")
}

func TestStaleSourceResetRecovers(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{}, sourceStale, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(100)
	assert.Equal(t, 1, h.resets)
	assert.Equal(t, secs(2, 5, 8, 10, 20, 40, 60), h.checks, "a recheck 3s after the reset")
	assert.Equal(t, []staleOutcome{{staleOutcomeRecoveredByReset, true}}, h.outcomes)
	assert.Zero(t, h.restarts)
}

func TestStaleSourceEscalatesToRestart(t *testing.T) {
	guard := &staleSourceGuard{}
	h := newStaleHarness(guard, sourceStale, sourceStale, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(3)
	assert.Equal(t, 1, h.resets)
	assert.Equal(t, 1, h.restarts)
	assert.Empty(t, h.outcomes, "the outcome is the next check's")

	// The restarted tunnel's checker, healthy.
	next := newStaleHarness(guard)
	next.t0 = h.t0.Add(10 * time.Second)
	next.m.update(next.t0, true)
	next.runChecks(100)
	assert.Equal(t, []staleOutcome{{staleOutcomeRecoveredByRestart, true}}, next.outcomes)
}

func TestStaleSourceNotRecoveredStopsUntilUpdate(t *testing.T) {
	guard := &staleSourceGuard{}
	h := newStaleHarness(guard, sourceStale, sourceStale, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(3)
	require.Equal(t, 1, h.restarts)

	next := newStaleHarness(guard, sourceStale, sourceStale, sourceStale, sourceStale, sourceStale, sourceStale)
	next.t0 = h.t0.Add(10 * time.Second)
	next.m.update(next.t0, true)
	next.runChecks(100)
	assert.Equal(t, []staleOutcome{{staleOutcomeNotRecovered, true}}, next.outcomes)
	assert.Zero(t, next.resets, "no recovery after a failed restart")
	assert.Zero(t, next.restarts)
	assert.Equal(t, 1, bytes.Count(next.logs.Bytes(), []byte("recovery failed")), "logged once")

	// A new update starts recovering again.
	next.results = []sourceStatus{sourceStale, sourceStale}
	next.m.update(next.t0.Add(time.Minute), true)
	next.runChecks(2)
	assert.Equal(t, 1, next.resets)
}

func TestStaleSourceRestartRateLimited(t *testing.T) {
	guard := &staleSourceGuard{}
	h := newStaleHarness(guard, sourceStale, sourceStale, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(3)
	require.Equal(t, 1, h.restarts)
	// The restart's outcome lapses unseen.
	guard.takePending(h.t0.Add(time.Hour))

	later := newStaleHarness(guard, sourceStale, sourceStale, sourceStale, sourceStale)
	later.t0 = h.t0.Add(5 * time.Minute)
	later.m.update(later.t0, true)
	later.runChecks(100)
	assert.Equal(t, 1, later.resets)
	assert.Zero(t, later.restarts)
	assert.Equal(t, []staleOutcome{{staleOutcomeRestartRateLimited, true}}, later.outcomes)

	after := newStaleHarness(guard, sourceStale, sourceStale, sourceStale)
	after.t0 = h.t0.Add(11 * time.Minute)
	after.m.update(after.t0, true)
	after.runChecks(3)
	assert.Equal(t, 1, after.restarts, "allowed again after 10 minutes")
}

func TestStaleSourceNoRestartPath(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{}, sourceStale, sourceStale, sourceStale)
	h.m.restart = nil
	h.m.update(h.t0, true)
	h.runChecks(100)
	assert.Equal(t, []staleOutcome{{staleOutcomeNotRecovered, true}}, h.outcomes)
}

func TestStaleSourceHealthyEndsEpisode(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{}, sourceStale, sourceStale, sourceHealthy, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(100)
	assert.Equal(t, 1, h.resets, "the stale check after recovery starts a new count")
	assert.Equal(t, []staleOutcome{{staleOutcomeRecoveredByReset, true}}, h.outcomes)
}

func TestStaleSourceLogFields(t *testing.T) {
	h := newStaleHarness(&staleSourceGuard{}, sourceStale)
	h.m.update(h.t0, true)
	h.runChecks(1)
	line := h.logs.String()
	for _, want := range []string{"iface=en0", "iface_index=4", "addrs=.20", "source=.22", "bound=ok", "since_update_s=2", "step=none", "result=stale"} {
		assert.Contains(t, line, want)
	}
}

func TestLastOctet(t *testing.T) {
	assert.Equal(t, ".22", lastOctet(netip.MustParseAddr("192.0.2.22")))
	assert.Equal(t, "non_ipv4", lastOctet(netip.MustParseAddr("2001:db8::1")))
}

type fakeInterfaceMonitor struct {
	mu        sync.Mutex
	iface     *control.Interface
	callbacks list.List[tun.DefaultInterfaceUpdateCallback]
}

func (f *fakeInterfaceMonitor) DefaultInterface() *control.Interface {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.iface
}

func (f *fakeInterfaceMonitor) RegisterCallback(cb tun.DefaultInterfaceUpdateCallback) *list.Element[tun.DefaultInterfaceUpdateCallback] {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callbacks.PushBack(cb)
}

func (f *fakeInterfaceMonitor) UnregisterCallback(e *list.Element[tun.DefaultInterfaceUpdateCallback]) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callbacks.Remove(e)
}

func (f *fakeInterfaceMonitor) set(iface *control.Interface) {
	f.mu.Lock()
	f.iface = iface
	callbacks := f.callbacks.Array()
	f.mu.Unlock()
	for _, cb := range callbacks {
		cb(iface, 0)
	}
}

func (f *fakeInterfaceMonitor) registered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callbacks.Len()
}

func TestStaleSourceCheckerLifecycle(t *testing.T) {
	monitor := &fakeInterfaceMonitor{}
	var checks atomic.Int32
	m := &staleSourceMachine{
		schedule:     []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 15 * time.Millisecond},
		recheckDelay: time.Millisecond,
		defaultIface: monitor.DefaultInterface,
		check: func(iface *control.Interface) sourceCheckResult {
			checks.Add(1)
			return sourceCheckResult{status: sourceHealthy}
		},
		reset:  func() {},
		guard:  &staleSourceGuard{},
		record: func(string, bool) {},
		logger: slog.New(slog.DiscardHandler),
	}
	closer := startStaleSourceChecker(context.Background(), monitor, m)
	require.Equal(t, 1, monitor.registered())

	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, checks.Load(), "nothing runs without a default interface")

	monitor.set(&control.Interface{Name: "en0", Index: 4})
	require.Eventually(t, func() bool { return checks.Load() == 3 }, time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 3, checks.Load(), "quiet after the schedule")

	monitor.set(&control.Interface{Name: "en0", Index: 4})
	require.Eventually(t, func() bool { return checks.Load() == 6 }, time.Second, time.Millisecond)

	// Close mid-schedule: it returns, unregisters, and nothing runs after.
	monitor.set(&control.Interface{Name: "en0", Index: 4})
	require.NoError(t, closer.Close())
	n := checks.Load()
	assert.Zero(t, monitor.registered())
	monitor.set(&control.Interface{Name: "en0", Index: 4})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, n, checks.Load())
}

func TestStaleSourceCheckerStartsWithDefaultInterface(t *testing.T) {
	monitor := &fakeInterfaceMonitor{iface: &control.Interface{Name: "en0", Index: 4}}
	var checks atomic.Int32
	m := &staleSourceMachine{
		schedule:     []time.Duration{time.Millisecond},
		defaultIface: monitor.DefaultInterface,
		check: func(*control.Interface) sourceCheckResult {
			checks.Add(1)
			return sourceCheckResult{status: sourceHealthy}
		},
		guard:  &staleSourceGuard{},
		logger: slog.New(slog.DiscardHandler),
	}
	ctx, cancel := context.WithCancel(context.Background())
	closer := startStaleSourceChecker(ctx, monitor, m)
	require.Eventually(t, func() bool { return checks.Load() == 1 }, time.Second, time.Millisecond)
	// Canceling the tunnel's context ends the goroutine too; Close then returns at once.
	cancel()
	require.NoError(t, closer.Close())
}

func TestStaleSourceRestartNeedsPlatform(t *testing.T) {
	c := NewVPNClient(t.TempDir(), slog.New(slog.DiscardHandler), nil)
	assert.Nil(t, c.staleSourceRestart(), "the direct restart path needs the backend's BoxOptions")
	tn := c.newTunnel(BoxOptions{}, "")
	assert.Same(t, &c.staleSource, tn.staleSource)
}
