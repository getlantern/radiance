package unbounded

import (
	"bytes"
	"log/slog"
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

// syncBuffer is a bytes.Buffer safe to log into from the manager's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	logs := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	return logs
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
	logs := captureLogs(t)
	resetManager(t, idleWidgets)
	primeManager(t, nil)
	manager.mu.Lock()
	manager.armed = false
	manager.mu.Unlock()

	require.NoError(t, Apply())
	s := CurrentSnapshot()
	assert.False(t, s.Running)
	assert.Equal(t, BlockedNotArmed, s.Blocked)
	assert.Contains(t, logs.String(), `msg="Unbounded: widget not running" reason=not_armed`)
}

// The reason is logged once per change, not on every evaluation.
func TestNoteBlocked_LogsOncePerChange(t *testing.T) {
	logs := captureLogs(t)
	resetManager(t, idleWidgets)
	require.NoError(t, settings.Set(settings.UnboundedKey, true))

	manager.noteBlocked()
	manager.noteBlocked()
	manager.mu.Lock()
	manager.lastCfg = testCfg()
	manager.mu.Unlock()
	manager.noteBlocked()

	assert.Equal(t, 1, strings.Count(logs.String(), "reason=no_config"), logs.String())
	assert.Equal(t, 1, strings.Count(logs.String(), "reason=feature_off"), logs.String())
}

// Once the widget runs it reports no reason, and a later block is logged
// again even if it's the reason logged before it ran.
func TestBlocked_ClearsWhileRunning(t *testing.T) {
	logs := captureLogs(t)
	resetManager(t, idleWidgets)
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
	assert.Equal(t, 2, strings.Count(logs.String(), "reason=no_config"), logs.String())
}
