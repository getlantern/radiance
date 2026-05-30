package unbounded

import (
	"context"
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
		{"feature+cfg, no toggle", false, true, &C.UnboundedConfig{}, false},
		{"toggle+feature, no cfg", true, true, nil, false},
		{"toggle+cfg, no feature", true, false, &C.UnboundedConfig{}, false},
		{"all three", true, true, &C.UnboundedConfig{}, true},
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
	manager.lastCfg = &C.UnboundedConfig{}
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
	manager.lastCfg = &C.UnboundedConfig{}
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
	manager.lastCfg = &C.UnboundedConfig{}
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

	// Start the first widget directly via manager.start.
	manager.start(&C.UnboundedConfig{})
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
		manager.start(&C.UnboundedConfig{})
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
		Unbounded: &C.UnboundedConfig{},
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
		Unbounded: &C.UnboundedConfig{},
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

	// First config — bring up widget #1.
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: &C.UnboundedConfig{DiscoverySrv: "https://a.example/"},
	})
	waitForRunning(t, manager, true, 1*time.Second)
	waitForCount(t, &starts, 1, 1*time.Second)

	// Same config — no restart.
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: &C.UnboundedConfig{DiscoverySrv: "https://a.example/"},
	})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), starts.Load(), "identical config must not restart")

	// Changed config — restart with new params.
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: &C.UnboundedConfig{DiscoverySrv: "https://b.example/"},
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
	manager.start(&C.UnboundedConfig{})
	waitForCount(t, &starts, 1, 1*time.Second)

	require.NoError(t, Stop(context.Background()))
	manager.mu.Lock()
	armed := manager.armed
	manager.mu.Unlock()
	require.False(t, armed, "Stop must disarm the manager")

	// All three start paths must be no-ops now.
	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	manager.mu.Lock()
	manager.lastFeatureOn = true
	manager.lastCfg = &C.UnboundedConfig{}
	manager.mu.Unlock()

	require.NoError(t, Apply())
	applyConfig(config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: &C.UnboundedConfig{},
	})
	manager.start(&C.UnboundedConfig{})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), starts.Load(), "no start path may revive the widget post-Stop")

	// A fresh InitSubscription re-arms — Start-after-Close path.
	initOnce = sync.Once{} // simulate re-arm path; InitSubscription's once guards re-subscription
	InitSubscription(&config.Config{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: &C.UnboundedConfig{},
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

	manager.start(&C.UnboundedConfig{})
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
		manager.start(&C.UnboundedConfig{})
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
	manager.lastCfg = &C.UnboundedConfig{}
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
