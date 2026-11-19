package vpn

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/events"
)

// A reader woken by an AutoSelectedEvent must find that selection already announced. The
// debug hook runs inside Emit, so it sees the state at the moment of the emit.
func TestAnnounceAutoSelectedStoresBeforeEmit(t *testing.T) {
	c := &VPNClient{}
	var atEmit *string
	events.SetEmitDebugLogger(func(eventType reflect.Type, _ int) {
		if eventType == reflect.TypeFor[AutoSelectedEvent]() {
			atEmit = c.announcedAuto.Load()
		}
	})
	t.Cleanup(func() { events.SetEmitDebugLogger(nil) })

	c.announceAutoSelected("fastest")

	require.NotNil(t, atEmit, "nothing announced at emit")
	assert.Equal(t, "fastest", *atEmit)
}
