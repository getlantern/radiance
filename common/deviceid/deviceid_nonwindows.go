//go:build !windows
// +build !windows

package deviceid

import (
	"github.com/getlantern/radiance/common/settings"
)

// Get returns a unique identifier for this device. The identifier is a random UUID. If unable to
// create a random UUID, this defaults to the old-style device ID derived from MAC address.
func Get() string {
	existingID := settings.GetString(settings.DeviceIDKey)
	if existingID != "" {
		return existingID
	}
	return newDeviceID()
}
