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
	t.Run("returns existing device ID when present", func(t *testing.T) {
		// Setup
		existingID := "existing-device-id-123"
		settings.Set(settings.DeviceIDKey, existingID)
		defer settings.Set(settings.DeviceIDKey, "")

		// Execute
		result := Get()

		// Assert
		assert.Equal(t, existingID, result)
	})

	t.Run("generates new device ID when not present", func(t *testing.T) {
		// Setup
		settings.Set(settings.DeviceIDKey, "")

		// Execute
		result := Get()

		// Assert
		assert.NotEmpty(t, result)
		// Verify it's a valid UUID format (36 characters with hyphens)
		assert.Len(t, result, 36)
		assert.Contains(t, result, "-")
	})

	t.Run("returns empty string for empty existing ID", func(t *testing.T) {
		// Setup
		settings.Set(settings.DeviceIDKey, "")

		// Execute
		result := Get()

		// Assert
		require.NotEmpty(t, result, "should generate a new ID when existing ID is empty")
	})
}
