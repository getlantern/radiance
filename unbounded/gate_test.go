package unbounded

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlantern/broflake/clientcore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
)

func idleWidgets(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
	return &fakeWidget{}, nil
}

// blockedLog records the reasons the manager logs.
type blockedLog struct {
	mu      sync.Mutex
	reasons []string
}

func (l *blockedLog) count(reason string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.reasons {
		if r == reason {
			n++
		}
	}
	return n
}

func (l *blockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.reasons, ",")
}

func captureBlocked(t *testing.T) *blockedLog {
	t.Helper()
	l := &blockedLog{}
	prev := logBlocked
	logBlocked = func(reason string) {
		l.mu.Lock()
		l.reasons = append(l.reasons, reason)
		l.mu.Unlock()
	}
	t.Cleanup(func() { logBlocked = prev })
	return l
}

func TestBlocked_Reasons(t *testing.T) {
	incomplete := testCfg()
	incomplete.EgressAddr = ""
	for _, tc := range []struct {
		name    string
		enabled bool
		armed   bool
		cfg     *C.UnboundedConfig
		feature bool
		want    string
	}{
		{"toggle off", false, true, testCfg(), true, BlockedDisabled},
		{"not armed", true, false, testCfg(), true, BlockedNotArmed},
		{"no config", true, true, nil, true, BlockedNoConfig},
		{"feature off", true, true, testCfg(), false, BlockedFeatureOff},
		{"config incomplete", true, true, incomplete, true, BlockedConfigIncomplete},
		{"nothing started it", true, true, testCfg(), true, BlockedNotStarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetManager(t, idleWidgets)
			require.NoError(t, settings.Set(settings.UnboundedKey, tc.enabled))
			manager.mu.Lock()
			manager.armed = tc.armed
			manager.lastCfg = tc.cfg
			manager.lastFeatureOn = tc.feature
			manager.mu.Unlock()
			assert.Equal(t, tc.want, CurrentSnapshot().Blocked)
		})
	}
}

// A backend that never armed the widget, with sharing on and a usable config,
// reports and logs not_armed, and Apply can't start it.
func TestBlocked_NotArmedIsVisible(t *testing.T) {
	logs := captureBlocked(t)
	resetManager(t, idleWidgets)
	primeManager(t, nil)
	manager.mu.Lock()
	manager.armed = false
	manager.mu.Unlock()

	require.NoError(t, Apply())
	s := CurrentSnapshot()
	assert.False(t, s.Running)
	assert.Equal(t, BlockedNotArmed, s.Blocked)
	assert.Equal(t, 1, logs.count(BlockedNotArmed), logs.String())
}

// The reason is logged once per change, not on every evaluation.
func TestNoteBlocked_LogsOncePerChange(t *testing.T) {
	logs := captureBlocked(t)
	resetManager(t, idleWidgets)
	require.NoError(t, settings.Set(settings.UnboundedKey, true))

	manager.noteBlocked()
	manager.noteBlocked()
	manager.mu.Lock()
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	manager.noteBlocked()

	assert.Equal(t, 1, logs.count(BlockedNoConfig), logs.String())
	assert.Equal(t, 1, logs.count(BlockedFeatureOff), logs.String())
}

// Once the widget runs it reports no reason, and a later block is logged
// again even if it's the reason logged before it ran.
func TestBlocked_ClearsWhileRunning(t *testing.T) {
	logs := captureBlocked(t)
	resetManager(t, idleWidgets)
	// Registered after resetManager, so it runs first: a failed assertion
	// mustn't leave resetManager waiting on a worker nothing stopped.
	t.Cleanup(manager.stop)
	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	manager.noteBlocked() // no_config

	applyConfig(config.Config{Features: map[string]bool{C.UNBOUNDED: true}, Unbounded: testCfg()})
	waitForRunning(t, manager, true, 2*time.Second)
	require.Eventually(t, func() bool { return CurrentSnapshot().Running }, 2*time.Second, 10*time.Millisecond)
	assert.Empty(t, CurrentSnapshot().Blocked)

	manager.stop()
	manager.mu.Lock()
	manager.lastCfg = nil
	manager.mu.Unlock()
	manager.noteBlocked()
	assert.Equal(t, 2, logs.count(BlockedNoConfig), logs.String())
}

// A cold start with no cached config says so, and a config the server turned
// the feature off in says that.
func TestBlocked_LoggedFromStartupAndConfig(t *testing.T) {
	logs := captureBlocked(t)
	resetManager(t, idleWidgets)
	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	manager.mu.Lock()
	manager.armed = false
	manager.mu.Unlock()

	InitSubscription(nil)
	assert.Equal(t, 1, logs.count(BlockedNoConfig), logs.String())

	applyConfig(config.Config{Features: map[string]bool{C.UNBOUNDED: false}, Unbounded: testCfg()})
	assert.Equal(t, 1, logs.count(BlockedFeatureOff), logs.String())
}

// The reason reaches IPC clients in the snapshot's JSON.
func TestSnapshot_BlockedOnTheWire(t *testing.T) {
	resetManager(t, idleWidgets)
	require.NoError(t, settings.Set(settings.UnboundedKey, true))
	b, err := json.Marshal(CurrentSnapshot())
	require.NoError(t, err)
	assert.Contains(t, string(b), `"blocked":"no_config"`)
}

// A worker whose stop outlives the timeout is tearing down, not starting: it
// reports why nothing runs, and the reason is logged once it exits.
func TestBlocked_StoppingWorkerIsNotStarting(t *testing.T) {
	prevTimeout := internalStopTimeout
	internalStopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { internalStopTimeout = prevTimeout })
	block := make(chan struct{})
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		return &fakeWidget{stopBlock: block}, nil
	})
	// Both run before resetManager's wait on the worker, so a failed
	// assertion can't leave it running: release unpins the widget's Stop,
	// then stop cancels the worker.
	t.Cleanup(manager.stop)
	release := sync.OnceFunc(func() { close(block) })
	t.Cleanup(release)
	primeManager(t, nil)
	require.NoError(t, Apply())
	waitForRunning(t, manager, true, time.Second)
	require.Eventually(t, func() bool { return CurrentSnapshot().Running }, time.Second, 10*time.Millisecond)

	logs := captureBlocked(t)
	require.NoError(t, settings.Set(settings.UnboundedKey, false))
	require.NoError(t, Apply()) // the stop times out on the pinned widget
	assert.Equal(t, BlockedDisabled, CurrentSnapshot().Blocked)

	release()
	waitForRunning(t, manager, false, time.Second)
	assert.Eventually(t, func() bool {
		return logs.count(BlockedDisabled) == 1
	}, time.Second, 10*time.Millisecond, logs.String())
}

// A widget that fails to build leaves nothing running with every condition
// met. Only the worker's exit sees that, so it must log not_started.
func TestBlocked_LoggedWhenTheWidgetFailsToBuild(t *testing.T) {
	logs := captureBlocked(t)
	// The build fails only after Apply returns, so Apply's own evaluation
	// sees the widget starting and only the worker's exit can log the failure.
	fail := make(chan struct{})
	resetManager(t, func(*clientcore.BroflakeOptions, *clientcore.WebRTCOptions, *clientcore.EgressOptions) (widget, error) {
		<-fail
		return nil, errors.New("no widget")
	})
	release := sync.OnceFunc(func() { close(fail) })
	t.Cleanup(release) // before resetManager's wait on the worker
	primeManager(t, nil)
	require.NoError(t, Apply())
	assert.Zero(t, logs.count(BlockedNotStarted), "logged before the build failed: %s", logs.String())

	release()
	assert.Eventually(t, func() bool { return logs.count(BlockedNotStarted) == 1 }, time.Second, 10*time.Millisecond, logs.String())
	assert.Equal(t, BlockedNotStarted, CurrentSnapshot().Blocked)
}
