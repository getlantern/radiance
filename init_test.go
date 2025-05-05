package radiance

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getlantern/radiance/client"
)

func TestInitialize(t *testing.T) {
	tmp := t.TempDir()
	opts := client.Options{
		DataDir:  tmp,
		LogDir:   tmp,
		Locale:   "en-US",
		DeviceID: "test-device-id",
	}
	initial, err := initialize(opts)
	assert.NoError(t, err)
	assert.NotNil(t, initial)

	newShared, err := initialize(opts)
	assert.NoError(t, err)
	assert.Equal(t, initial, newShared, "Expected the same instance to be returned on subsequent calls")
}
