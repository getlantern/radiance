//go:build !windows
// +build !windows

package deviceid

import (
	"testing"

	"github.com/getlantern/radiance/common/settings"
	"github.com/stretchr/testify/assert"
)

func TestGet(t *testing.T) {
	tests := []struct {
		name            string
		existingID      string
		expectNewID     bool
		setupSettings   func()
		cleanupSettings func()
	}{
		{
			name:        "returns existing device ID when present",
			existingID:  "existing-device-id-12345",
			expectNewID: false,
			setupSettings: func() {
				settings.Set(settings.DeviceIDKey, "existing-device-id-12345")
			},
			cleanupSettings: func() {
				settings.Set(settings.DeviceIDKey, "")
			},
		},
		{
			name:        "generates new device ID when none exists",
			existingID:  "",
			expectNewID: true,
			setupSettings: func() {
				settings.Set(settings.DeviceIDKey, "")
			},
			cleanupSettings: func() {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupSettings()
			defer tt.cleanupSettings()

			result := Get()

			assert.NotEmpty(t, result)
			if tt.expectNewID {
				// New device ID should be generated
				assert.NotEqual(t, tt.existingID, result)
			} else {
				// Should return the existing ID
				assert.Equal(t, tt.existingID, result)
			}
		})
	}
}

func TestGetConsistency(t *testing.T) {
	// Clear any existing device ID
	settings.Set(settings.DeviceIDKey, "")
	defer settings.Set(settings.DeviceIDKey, "")

	// First call should generate a new ID
	firstID := Get()
	assert.NotEmpty(t, firstID)

	// Set the generated ID as if it was saved
	settings.Set(settings.DeviceIDKey, firstID)

	// Second call should return the same ID
	secondID := Get()
	assert.Equal(t, firstID, secondID)
}
