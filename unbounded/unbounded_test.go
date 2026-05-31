package unbounded

import (
	"context"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/getlantern/common"
	"github.com/getlantern/broflake/clientcore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
)

// TestMain initializes the settings package once for the whole test
// binary. settings.InitSettings is itself a sync.Once-guarded
// installer (it persists the path to k.filePath), so calling it per
// t.TempDir() leaves the settings layer pointing at a directory the
// testing infra has already cleaned up by the time the second test
// runs — every subsequent settings.Set then fails with ENOENT.
//
// os.Exit bypasses deferred calls, so the tmp-dir cleanup is done
// explicitly: capture m.Run's exit code, RemoveAll, then exit with
// that code. A naked defer + os.Exit would silently leak a directory
// per test invocation.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "radiance-unbounded-settings-*")
	if err != nil {
		panic(err)
	}
	if err := settings.InitSettings(dir); err != nil {
		os.RemoveAll(dir)
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeWidget is a stand-in for the live broflake UI. stopBlock, if
// non-nil, lets a test pin Stop() until the test releases it — used
// to drive the "stop must wait for the worker" assertions.
type fakeWidget struct {
	stopCalled atomic.Int32
	stopBlock  chan struct{}
}

func (w *fakeWidget) Stop() {
	w.stopCalled.Add(1)
	if w.stopBlock != nil {
		<-w.stopBlock
	}
}

// resetManager swaps the package-level manager + widget factory and
// resets the UnboundedKey setting. Cleanup restores everything so
// tests don't bleed into each other.
func resetManager(t *testing.T, build func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error)) {
	t.Helper()
	require.NoError(t, settings.Set(settings.UnboundedKey, false))

	prevManager := manager
	prevWidget := newWidget
	prevInit := initOnce
	// armed: true so direct manager.start calls in tests don't bail on
	// the disarmed gate. Tests that exercise Stop's disarm behavior
	// (TestStopDisarmsManager) flip it explicitly.
	manager = &unboundedManager{armed: true}
	newWidget = build
	initOnce = sync.Once{}
	t.Cleanup(func() {
		// Wait for any still-live worker on the test's manager to
		// exit before swapping the package-level newWidget back.
		// Without this, the worker's read of newWidget (when the
		// fake created it) races with the cleanup's write at test
		// teardown — the race detector flags it even though the
		// worker has already finished its call. Tests that leave a
		// pinned worker (e.g. TestStopCtx_TimesOut) must release
		// it before returning so this wait completes.
		manager.mu.Lock()
		done := manager.done
		manager.mu.Unlock()
		if done != nil {
			<-done
		}
		manager = prevManager
		newWidget = prevWidget
		initOnce = prevInit
		_ = settings.Set(settings.UnboundedKey, false)
	})
}

// waitForRunning polls m.cancel under m.mu until it matches expected,
// primeManager seeds the predicate fields so a direct manager.start()
// in a test will satisfy the shouldStart() check inside the lock: the
// UnboundedKey setting goes true, manager.lastFeatureOn = true, and
// manager.lastCfg = the supplied cfg (or testCfg if nil). Tests that
// exercise the start path directly call this once after resetManager.
//
// The default test cfg populates all four required URL fields so
// cfgUsable passes — a zero-value UnboundedConfig would fail the
// "all fields supplied" gate and start() would bail.
func primeManager(t *testing.T, cfg *C.UnboundedConfig) {
	t.Helper()
	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	if cfg == nil {
		cfg = testCfg()
	}
	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = cfg
	manager.mu.Unlock()
}

// testCfg returns a UnboundedConfig that passes cfgUsable. Use this
// instead of a bare `&C.UnboundedConfig{}` literal in tests; the
// zero-value literal would now fail the runnable predicate.
func testCfg() *C.UnboundedConfig {
	return &C.UnboundedConfig{
		DiscoverySrv:      "https://discovery.test.example",
		DiscoveryEndpoint: "/v1/disco",
		EgressAddr:        "https://egress.test.example",
		EgressEndpoint:    "/v1/egress",
	}
}

// or the deadline expires. The start goroutine sets m.cancel under
// m.mu before kicking off the worker, so this is a sufficient signal
// that a transition has been requested — note that for "true" the
// worker's newWidget call may still be pending.
func waitForRunning(t *testing.T, m *unboundedManager, expected bool, dur time.Duration) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		running := m.cancel != nil
		m.mu.Unlock()
		if running == expected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForRunning: timed out waiting for running=%v", expected)
}

// waitForCount polls an int32 atomic until it equals want, or the
// deadline expires. Used in place of a flat-sleep when waiting for
// the start goroutine to call newWidget.
func waitForCount(t *testing.T, v *atomic.Int32, want int32, dur time.Duration) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		if v.Load() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForCount: timed out waiting for count=%d, got %d", want, v.Load())
}

func TestShouldStart(t *testing.T) {
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		return &fakeWidget{}, nil
	})

	tests := []struct {
		name    string
		toggle  bool
		feature bool
		cfg     *C.UnboundedConfig
		want    bool
	}{
		{"all off", false, false, nil, false},
		{"toggle only", true, false, nil, false},
		{"feature+cfg, no toggle", false, true, testCfg(), false},
		{"toggle+feature, no cfg", true, true, nil, false},
		{"toggle+cfg, no feature", true, false, testCfg(), false},
		{"all three", true, true, testCfg(), true},
		// Partial cfg (missing required URLs) treated as not-yet-ready
		// — broflake's client defaults would otherwise route real
		// traffic through upstream-maintainer infra, bypassing the
		// server's per-environment endpoint selection.
		{"partial cfg, no egress", true, true,
			&C.UnboundedConfig{
				DiscoverySrv:      "https://d.example",
				DiscoveryEndpoint: "/v1/disco",
			}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, settings.Set(settings.UnboundedKey, tc.toggle))
			manager.mu.Lock()
			manager.lastFeatureOn = tc.feature
			manager.lastCfg = tc.cfg
			got := manager.shouldStart()
			manager.mu.Unlock()
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestApply_DisabledIsNoop: Apply() returns immediately when the
// local toggle is off, regardless of cached server state.
func TestApply_DisabledIsNoop(t *testing.T) {
	starts := atomic.Int32{}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		starts.Add(1)
		return &fakeWidget{}, nil
	})

	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	require.NoError(t, Apply())

	// Allow any spurious goroutine a beat to land.
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), starts.Load(), "Apply() must not start widget when toggle is off")
}

// TestApply_StartsWhenAllConditionsHold: with toggle on + cached
// feature flag + cached config, Apply() spins up exactly one widget.
func TestApply_StartsWhenAllConditionsHold(t *testing.T) {
	fw := &fakeWidget{}
	starts := atomic.Int32{}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		starts.Add(1)
		return fw, nil
	})

	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	require.NoError(t, Apply())

	waitForRunning(t, manager, true, 1*time.Second)
	waitForCount(t, &starts, 1, 1*time.Second)

	// Double-Apply should not start a second widget.
	require.NoError(t, Apply())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(1), starts.Load())

	// Tear down.
	require.NoError(t, settings.Set(settings.UnboundedKey, false))
	require.NoError(t, Apply())
	waitForRunning(t, manager, false, 1*time.Second)
	assert.Equal(t, int32(1), fw.stopCalled.Load())
}

// TestStop_WaitsForWorker: stop() blocks until the worker's ui.Stop
// returns. Pin ui.Stop with stopBlock and observe stop()'s wait.
func TestStop_WaitsForWorker(t *testing.T) {
	block := make(chan struct{})
	fw := &fakeWidget{stopBlock: block}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		return fw, nil
	})

	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	require.NoError(t, Apply())
	waitForRunning(t, manager, true, 1*time.Second)

	stopReturned := make(chan struct{})
	go func() {
		manager.stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
		t.Fatal("stop() returned before fake widget's Stop unblocked")
	case <-time.After(100 * time.Millisecond):
	}
	close(block)
	select {
	case <-stopReturned:
	case <-time.After(1 * time.Second):
		t.Fatal("stop() did not return after fake widget's Stop unblocked")
	}
	assert.Equal(t, int32(1), fw.stopCalled.Load())
}

// TestStartDuringStop_NoOverlap pins the transitionMu invariant: at
// any instant, at most one broflake widget is between newWidget and
// Stop's return. If stop() merely signalled cancel and returned
// (without holding transitionMu through the wait on done), a
// concurrent start() could observe m.cancel == nil and bring up a
// second widget while the first is still inside ui.Stop. The test
// exercises the manager directly (skipping Apply's predicate check)
// because the property being verified is local to start/stop
// serialization — two widgets alive simultaneously means
// transitionMu failed to serialize.
func TestStartDuringStop_NoOverlap(t *testing.T) {
	var (
		liveCount atomic.Int32
		maxLive   atomic.Int32
		stopGate  = make(chan struct{})
	)
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		v := liveCount.Add(1)
		if v > maxLive.Load() {
			maxLive.Store(v)
		}
		return &countingWidget{onStop: func() {
			<-stopGate
			liveCount.Add(-1)
		}}, nil
	})

	// Prime predicate so direct manager.start calls satisfy
	// shouldStart() inside the lock, then start the first widget.
	primeManager(t, nil)
	manager.start()
	waitForCount(t, &liveCount, 1, 1*time.Second)

	// Kick off a stop — it'll block on stopGate inside the fake's
	// onStop until we release it.
	stopDone := make(chan struct{})
	go func() {
		manager.stop()
		close(stopDone)
	}()

	// Once the stop is in flight (worker has received cancel and
	// entered onStop), launch a concurrent start. transitionMu must
	// hold this until the prior stop returns.
	time.Sleep(50 * time.Millisecond)
	startDone := make(chan struct{})
	go func() {
		manager.start()
		close(startDone)
	}()

	// Neither should have completed yet.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-stopDone:
		t.Fatal("stop returned before stopGate released")
	case <-startDone:
		t.Fatal("start returned before prior stop completed")
	default:
	}

	// Release the first widget's Stop. stop() returns, transitionMu
	// frees, start() acquires it and creates widget #2.
	close(stopGate)
	select {
	case <-stopDone:
	case <-time.After(1 * time.Second):
		t.Fatal("stop did not return after gate release")
	}
	select {
	case <-startDone:
	case <-time.After(1 * time.Second):
		t.Fatal("start did not return after stop completed")
	}

	// Widget #2 should now be live.
	waitForCount(t, &liveCount, 1, 1*time.Second)
	require.Equal(t, int32(1), maxLive.Load(), "two widgets ran concurrently — transitionMu failed")

	// Cleanup: tear down widget #2. stopGate is already closed, so
	// the worker's Stop returns immediately.
	manager.stop()
	waitForCount(t, &liveCount, 0, 1*time.Second)
}

// TestInitSubscription_SeedsCachedConfig: passing a non-nil initial
// config to InitSubscription kicks off the same applyConfig path
// the live event subscriber takes. With all three conditions met,
// the widget should auto-start without a fresh NewConfigEvent.
func TestInitSubscription_SeedsCachedConfig(t *testing.T) {
	starts := atomic.Int32{}
	fw := &fakeWidget{}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		starts.Add(1)
		return fw, nil
	})

	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	cfg := &config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: testCfg(),
	}
	InitSubscription(cfg)

	waitForRunning(t, manager, true, 1*time.Second)
	waitForCount(t, &starts, 1, 1*time.Second)

	// Cleanup.
	require.NoError(t, settings.Set(settings.UnboundedKey, false))
	require.NoError(t, Apply())
	waitForRunning(t, manager, false, 1*time.Second)
}

// TestInitSubscription_FutureEventStillFires: even with a nil
// initial, the subscription still reacts to a subsequent
// NewConfigEvent — confirms the seed didn't replace the live path.
func TestInitSubscription_FutureEventStillFires(t *testing.T) {
	starts := atomic.Int32{}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		starts.Add(1)
		return &fakeWidget{}, nil
	})

	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	InitSubscription(nil)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), starts.Load(), "should not start with no cached config")

	// Fire a NewConfigEvent with all three conditions satisfied.
	cfg := config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: testCfg(),
	}
	events.Emit(config.NewConfigEvent{New: &cfg})
	waitForRunning(t, manager, true, 1*time.Second)
	waitForCount(t, &starts, 1, 1*time.Second)

	// Cleanup.
	require.NoError(t, settings.Set(settings.UnboundedKey, false))
	require.NoError(t, Apply())
	waitForRunning(t, manager, false, 1*time.Second)
}

// TestApplyConfig_RestartsOnParamChange: broflake consumes its
// options once in clientcore.NewBroflake. A server-side config
// change while the widget is alive must therefore tear down the
// current worker and bring up a new one — otherwise the running
// proxy stays on stale discovery/egress endpoints. applyConfig
// compares the new cfg against runningCfg (the snapshot the worker
// was started with) and triggers stop+start when they differ,
// provided the three-condition predicate still holds.
func TestApplyConfig_RestartsOnParamChange(t *testing.T) {
	starts := atomic.Int32{}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		starts.Add(1)
		return &fakeWidget{}, nil
	})

	require.NoError(t, settings.Set(settings.UnboundedKey, true))

	cfgA := testCfg()
	cfgB := testCfg()
	cfgB.DiscoverySrv = "https://b.example/"

	// First config — bring up widget #1.
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: cfgA,
	})
	waitForRunning(t, manager, true, 1*time.Second)
	waitForCount(t, &starts, 1, 1*time.Second)

	// Same config — no restart.
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: testCfg(), // value-equal to cfgA
	})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), starts.Load(), "identical config must not restart")

	// Changed config — restart with new params.
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: cfgB,
	})
	waitForCount(t, &starts, 2, 2*time.Second)

	// Cleanup.
	require.NoError(t, settings.Set(settings.UnboundedKey, false))
	require.NoError(t, Apply())
	waitForRunning(t, manager, false, 1*time.Second)
}

// TestStopDisarmsManager verifies the public-Stop shutdown
// contract: after Stop returns, NO subsequent start path may revive
// the widget — not Apply, not applyConfig from a late config event,
// not manager.start directly. The armed gate enforces this;
// transitionMu alone is not enough because it just serializes
// transitions, it doesn't decide whether a queued one should still
// proceed after a shutdown. Without this gate, a config event
// firing during Stop's wait-for-worker window would block at
// transitionMu and then start a fresh widget the moment Stop's
// caller returned from LocalBackend.Close, silently keeping
// broflake alive past the documented shutdown contract.
func TestStopDisarmsManager(t *testing.T) {
	starts := atomic.Int32{}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		starts.Add(1)
		return &fakeWidget{}, nil
	})

	// Bring up a widget so Stop has something to tear down.
	primeManager(t, nil)
	manager.start()
	waitForCount(t, &starts, 1, 1*time.Second)

	require.NoError(t, Stop(context.Background()))
	manager.mu.Lock()
	armed := manager.armed
	manager.mu.Unlock()
	require.False(t, armed, "Stop must disarm the manager")

	// All three start paths must be no-ops now. Predicate is still
	// satisfied (toggle, feature, cfg) — only armed gates the start.
	primeManager(t, nil)

	require.NoError(t, Apply())
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: testCfg(),
	})
	manager.start()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), starts.Load(), "no start path may revive the widget post-Stop")

	// A fresh InitSubscription re-arms — Start-after-Close path.
	initOnce = sync.Once{} // simulate re-arm path; InitSubscription's once guards re-subscription
	InitSubscription(&config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: testCfg(),
	})
	waitForCount(t, &starts, 2, 1*time.Second)

	// Cleanup.
	require.NoError(t, Stop(context.Background()))
}

// TestStartAfterStopWaiting_NoRevival: pin Stop in its <-done wait
// (worker can't exit because its Stop is blocked), then race a
// config-event-driven start against the disarm. Confirms that even a
// start queued at transitionMu while Stop is waiting bails out after
// transitionMu releases — the armed re-check inside start catches it.
func TestStartAfterStopWaiting_NoRevival(t *testing.T) {
	block := make(chan struct{})
	starts := atomic.Int32{}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		starts.Add(1)
		return &fakeWidget{stopBlock: block}, nil
	})

	primeManager(t, nil)
	manager.start()
	waitForCount(t, &starts, 1, 1*time.Second)

	// Stop in a goroutine — it'll set armed=false, signal cancel,
	// then block on <-done waiting for the worker, which is in turn
	// pinned by stopBlock.
	stopDone := make(chan struct{})
	go func() {
		_ = Stop(context.Background())
		close(stopDone)
	}()

	// Give Stop a beat to acquire transitionMu and disarm.
	time.Sleep(50 * time.Millisecond)
	manager.mu.Lock()
	armed := manager.armed
	manager.mu.Unlock()
	require.False(t, armed, "Stop must disarm before waiting on done")

	// Now race a start. It blocks at transitionMu until Stop returns.
	startDone := make(chan struct{})
	go func() {
		manager.start()
		close(startDone)
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-startDone:
		t.Fatal("start completed before Stop unblocked")
	default:
	}

	// Release the worker so Stop returns.
	close(block)
	select {
	case <-stopDone:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop did not return after worker released")
	}
	select {
	case <-startDone:
	case <-time.After(1 * time.Second):
		t.Fatal("start did not return after Stop returned")
	}

	// Critically: starts must still be 1 — the queued start saw
	// armed=false and bailed.
	assert.Equal(t, int32(1), starts.Load(), "start queued during Stop must not revive the widget")
}

// TestStopCtx_TimesOut: Stop(ctx) returns ctx.Err() when the worker
// is still mid-shutdown past the deadline. The worker is left to
// exit on its own schedule afterwards.
func TestStopCtx_TimesOut(t *testing.T) {
	block := make(chan struct{})
	fw := &fakeWidget{stopBlock: block}
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		return fw, nil
	})

	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	require.NoError(t, Apply())
	waitForRunning(t, manager, true, 1*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := Stop(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Release the worker so the test exits cleanly.
	close(block)
}

// TestConnectionEventBridge pins the observable API this package
// adds: broflake's OnConnectionChangeFunc callback must translate
// (state, workerIdx, addr) into a ConnectionEvent with the matching
// State, the addr.String() Source (empty when addr is nil), and a
// freshly-stamped Timestamp. Capture the callback that start
// installs on bfOpt via a fake newWidget, invoke it with both nil
// and non-nil addrs, then assert the events arriving via
// events.Subscribe.
func TestConnectionEventBridge(t *testing.T) {
	var captured atomic.Pointer[clientcore.ConnectionChangeFunc]
	resetManager(t, func(bfOpt *clientcore.BroflakeOptions, _ *clientcore.WebRTCOptions, _ *clientcore.EgressOptions) (widget, error) {
		cb := bfOpt.OnConnectionChangeFunc
		captured.Store(&cb)
		return &fakeWidget{}, nil
	})

	primeManager(t, nil)
	manager.start()
	// Worker may still be inside the goroutine setup when start
	// returns; wait until newWidget has been called and the callback
	// captured.
	deadline := time.Now().Add(1 * time.Second)
	for captured.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.NotNil(t, captured.Load(), "newWidget was never invoked")
	cb := *captured.Load()
	require.NotNil(t, cb, "OnConnectionChangeFunc must be installed on bfOpt")

	// Buffered enough that events.Emit's per-callback goroutines
	// can deposit before the test reads.
	ch := make(chan ConnectionEvent, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events.SubscribeContext(ctx, func(evt ConnectionEvent) { ch <- evt })

	before := time.Now().UnixMilli()
	cb(1, 7, net.ParseIP("198.51.100.42"))
	cb(-1, 7, nil)
	after := time.Now().UnixMilli()

	// events.Emit dispatches each subscriber on a per-callback
	// goroutine, so the two events can arrive in either order.
	// Assert set-membership keyed by State (unique in this test)
	// rather than positional equality.
	byState := make(map[int]ConnectionEvent, 2)
	for i := 0; i < 2; i++ {
		select {
		case evt := <-ch:
			byState[evt.State] = evt
		case <-time.After(1 * time.Second):
			t.Fatalf("timed out waiting for ConnectionEvent #%d", i+1)
		}
	}

	require.Contains(t, byState, 1, "expected an accept event (State=+1)")
	require.Contains(t, byState, -1, "expected a close event (State=-1)")
	assert.Equal(t, "198.51.100.42", byState[1].Source, "accept Source")
	assert.Equal(t, "", byState[-1].Source, "close Source (nil addr -> empty string)")
	for state, evt := range byState {
		assert.GreaterOrEqual(t, evt.Timestamp, before, "State=%d Timestamp not before emit", state)
		assert.LessOrEqual(t, evt.Timestamp, after, "State=%d Timestamp not after emit", state)
	}

	// Cleanup.
	require.NoError(t, Stop(context.Background()))
}

// TestInternalStop_TimesOut: internal stop (called by Apply and
// applyConfig) must not block forever when ui.Stop hangs. If it
// did, toggling Unbounded off via PatchSettings or a disabling
// config event would block the caller indefinitely while holding
// transitionMu. Pin ui.Stop with stopBlock past a short
// internalStopTimeout and confirm Apply returns within a sane
// budget.
func TestInternalStop_TimesOut(t *testing.T) {
	prevTimeout := internalStopTimeout
	internalStopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { internalStopTimeout = prevTimeout })

	block := make(chan struct{})
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		return &fakeWidget{stopBlock: block}, nil
	})

	primeManager(t, nil)
	require.NoError(t, Apply())
	waitForRunning(t, manager, true, 1*time.Second)

	// Flip toggle off so Apply's !Enabled branch calls manager.stop.
	// With the worker pinned by stopBlock, internal stop must time
	// out rather than hang. Apply itself returns nil — the timeout
	// is logged but not propagated.
	require.NoError(t, settings.Set(settings.UnboundedKey, false))
	applyReturned := make(chan struct{})
	go func() {
		_ = Apply()
		close(applyReturned)
	}()
	select {
	case <-applyReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Apply blocked past timeout — internal stop is not context-bounded")
	}

	// Release the worker so the test's cleanup wait completes.
	close(block)
}
