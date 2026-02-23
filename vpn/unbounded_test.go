package vpn

import (
	"testing"

	C "github.com/getlantern/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func initTestSettings(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	require.NoError(t, settings.InitSettings(tmp))
	t.Cleanup(settings.Reset)
}

func TestShouldRunUnbounded(t *testing.T) {
	initTestSettings(t)
	settings.Set(settings.UnboundedKey, false)

	cfg := C.ConfigResponse{
		Features:  map[string]bool{C.UNBOUNDED: true},
		Unbounded: &C.UnboundedConfig{},
	}

	assert.False(t, shouldRunUnbounded(cfg), "should be false when setting is off")

	settings.Set(settings.UnboundedKey, true)
	assert.True(t, shouldRunUnbounded(cfg), "should be true when all conditions met")

	// Missing feature flag
	cfg.Features[C.UNBOUNDED] = false
	assert.False(t, shouldRunUnbounded(cfg), "should be false when feature flag is off")
	cfg.Features[C.UNBOUNDED] = true

	// Missing config
	cfg.Unbounded = nil
	assert.False(t, shouldRunUnbounded(cfg), "should be false when config is nil")
}

func TestSetUnboundedToggle(t *testing.T) {
	initTestSettings(t)
	settings.Set(settings.UnboundedKey, false)

	require.NoError(t, SetUnbounded(true))
	assert.True(t, UnboundedEnabled())

	require.NoError(t, SetUnbounded(false))
	assert.False(t, UnboundedEnabled())

	// Idempotent
	require.NoError(t, SetUnbounded(false))
	assert.False(t, UnboundedEnabled())
}

func TestStopWhenNotRunning(t *testing.T) {
	unbounded.stop()
	assert.Nil(t, unbounded.cancel)
}

func TestStartStopLifecycle(t *testing.T) {
	unbounded.mu.Lock()
	unbounded.cancel = nil
	unbounded.mu.Unlock()

	unbounded.mu.Lock()
	assert.Nil(t, unbounded.cancel)
	unbounded.mu.Unlock()

	// stop is safe when already stopped
	unbounded.stop()
	unbounded.mu.Lock()
	assert.Nil(t, unbounded.cancel)
	unbounded.mu.Unlock()
}
