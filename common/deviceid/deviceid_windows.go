//go:build windows
// +build windows

package deviceid

import (
	"log/slog"

	"golang.org/x/sys/windows/registry"
)

const (
	keyPath = `Sofware\\Lantern`
)

// Get returns a unique identifier for this device. The identifier is a random UUID that's stored in the registry
// at HKEY_CURRENT_USERS\Software\Lantern\deviceid. If unable to read/write to the registry, this defaults to the
// old-style device ID derived from MAC address.
func Get() string {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE|registry.SET_VALUE|registry.WRITE)
	if err != nil {
		slog.Error("Unable to create registry entry to store deviceID, defaulting to old-style device ID: %v", "error", err)
		return OldStyleDeviceID()
	}

	existing, _, err := key.GetStringValue("deviceid")
	if err != nil {
		if err != registry.ErrNotExist {
			slog.Error("Unexpected error reading deviceID, default to old-style device ID:", "error", err)
			return OldStyleDeviceID()
		}
		slog.Debug("Storing new deviceID")
		deviceID := newDeviceID()
		err = key.SetStringValue("deviceid", deviceID)
		if err != nil {
			slog.Error("Error storing new deviceID, defaulting to old-style device IDL", "error", err)
			return OldStyleDeviceID()
		}
		return deviceID
	}

	return existing
}
