//go:build !windows
// +build !windows

package deviceid

import (
	"testing"

	"github.com/getlantern/radiance/common/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGet(t *testing.T) {
	settings.InitSettings(t.TempDir())
	// Save original setting and restore after test
	originalID := settings.GetString(settings.DeviceIDKey)
	defer func() {
		if originalID != "" {
			settings.Set(settings.DeviceIDKey, originalID)
		} else {
			settings.Set(settings.DeviceIDKey, "")
		}
	}()

	t.Run("returns existing device ID when present", func(t *testing.T) {
		existingID := "existing-device-id-123"
		err := settings.Set(settings.DeviceIDKey, existingID)
		require.NoError(t, err)

		result := Get()
		assert.Equal(t, existingID, result)
	})

	t.Run("generates new device ID when not present", func(t *testing.T) {
		settings.Set(settings.DeviceIDKey, "")

		result := Get()
		assert.NotEmpty(t, result)
		// Verify it's a valid UUID format (36 characters with dashes)
		assert.Len(t, result, 36)
		assert.Contains(t, result, "-")
	})

	t.Run("persists new device ID", func(t *testing.T) {
		settings.Set(settings.DeviceIDKey, "")

		firstCall := Get()
		secondCall := Get()

		// Second call should return the same ID that was persisted
		assert.Equal(t, firstCall, secondCall)
	})
}
