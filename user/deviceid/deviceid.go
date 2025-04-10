package deviceid

import (
	"encoding/base64"

	"github.com/getlantern/golog"
	"github.com/google/uuid"
)

var (
	log = golog.LoggerFor("deviceid")
)

// OldStyleDeviceID returns the old style of device ID, which is derived from the MAC address.
func OldStyleDeviceID() string {
	return base64.StdEncoding.EncodeToString(uuid.NodeID())
}
