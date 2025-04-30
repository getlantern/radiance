package radiance

import (
	"testing"

	"github.com/getlantern/radiance/client"
	"github.com/stretchr/testify/assert"
)

func TestInitialize(t *testing.T) {
	opts := client.Options{
		DataDir:  "/tmp/data",
		LogDir:   "/tmp/logs",
		Locale:   "en-US",
		DeviceID: "test-device-id",
	}
	_, err := initialize(opts)

	assert.NoError(t, err)
	assert.NotNil(t, sharedInit)
	newShared, ierr := initialize(opts)
	assert.NoError(t, ierr)
	assert.Equal(t, sharedInit, newShared)
}
