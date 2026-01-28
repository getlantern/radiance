//go:build !windows
// +build !windows

package deviceid

import (
	"log/slog"

	"github.com/getlantern/radiance/common/settings"
	"github.com/google/uuid"
)

// Get returns a unique identifier for this device. The identifier is a random UUID that's stored on
// disk. If unable to create a random UUID, this defaults to the old-style device ID derived from
// MAC address.
func Get() string {
	existingID := settings.GetString(settings.DeviceIDKey)
	if existingID != "" {
		return existingID
	}
	return newDeviceID()
}

func newDeviceID() string {
	newID, err := uuid.NewRandom()
	if err != nil {
		slog.Error("Error generating new deviceID, defaulting to old-style device ID", "error", err)
		return OldStyleDeviceID()
	}
	idStr := newID.String()
	if err := settings.Set(settings.DeviceIDKey, idStr); err != nil {
		slog.Error("Error persisting new deviceID", "error", err)
	}
	return idStr
}
