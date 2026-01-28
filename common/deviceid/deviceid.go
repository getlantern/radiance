package deviceid

import (
	"encoding/base64"
	"log/slog"

	"github.com/google/uuid"
)

// OldStyleDeviceID returns the old style of device ID, which is derived from the MAC address.
func OldStyleDeviceID() string {
	return base64.StdEncoding.EncodeToString(uuid.NodeID())
}

func newDeviceID() string {
	if newID, err := uuid.NewRandom(); err != nil {
		slog.Error("Error generating new deviceID, defaulting to old-style device ID", "error", err)
		return OldStyleDeviceID()
	} else {
		return newID.String()
	}
}
