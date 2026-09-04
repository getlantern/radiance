package vpn

import (
	"context"
	"testing"
	"time"

	"github.com/sagernet/sing/service/pause"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/events"
)

func TestOnPauseUpdate(t *testing.T) {
	// want == "" means no NetworkEvent should be emitted.
	cases := []struct {
		name string
		evt  int
		want NetworkEventType
	}{
		{"device paused", pause.EventDevicePaused, NetworkEventPaused},
		{"network paused", pause.EventNetworkPause, NetworkEventPaused},
		{"network wake", pause.EventNetworkWake, NetworkEventWake},
		{"device wake ignored", pause.EventDeviceWake, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			got := make(chan NetworkEventType, 1)
			events.SubscribeContext(ctx, func(evt NetworkEvent) { got <- evt.EventType })

			(&tunnel{}).onPauseUpdate(tc.evt)

			if tc.want == "" {
				select {
				case ev := <-got:
					t.Fatalf("expected no NetworkEvent, got %q", ev)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
			select {
			case ev := <-got:
				require.Equal(t, tc.want, ev)
			case <-time.After(2 * time.Second):
				t.Fatal("no NetworkEvent emitted")
			}
		})
	}
}
