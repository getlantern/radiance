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
	tun := &tunnel{}
	var atEmit *string
	events.SetEmitDebugLogger(func(eventType reflect.Type, _ int) {
		if eventType == reflect.TypeFor[AutoSelectedEvent]() {
			atEmit = tun.announcedAuto.Load()
		}
	})
	t.Cleanup(func() { events.SetEmitDebugLogger(nil) })

	announceAutoSelected(tun, "fastest")

	require.NotNil(t, atEmit, "nothing announced at emit")
	assert.Equal(t, "fastest", *atEmit)
}

func TestAnnouncedAutoSelectedServerIsPerTunnel(t *testing.T) {
	old := &tunnel{}
	c := &VPNClient{tunnel: old}
	c.status.Store(Connected)
	announceAutoSelected(old, "old-pick")
	require.Equal(t, "old-pick", c.AnnouncedAutoSelectedServer())

	// A reconnect replaces the tunnel; a listener that has not stopped yet can only
	// announce into the tunnel it read from.
	next := &tunnel{}
	c.mu.Lock()
	c.tunnel = next
	c.mu.Unlock()
	announceAutoSelected(old, "late-old-pick")
	assert.Empty(t, c.AnnouncedAutoSelectedServer())

	announceAutoSelected(next, "new-pick")
	assert.Equal(t, "new-pick", c.AnnouncedAutoSelectedServer())

	c.status.Store(Disconnected)
	assert.Empty(t, c.AnnouncedAutoSelectedServer())
}
