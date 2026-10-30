package common

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientIPHeader(t *testing.T) {
	t.Cleanup(func() { SetPublicIP("") })
	tests := []struct {
		name               string
		set                func()
		wantAll, wantExact string
	}{
		{"none", func() { SetPublicIP("") }, "", ""},
		{"stored", func() { SetStoredPublicIP("203.0.113.0") }, "203.0.113.0", ""},
		{"detected", func() { SetPublicIP("203.0.113.7") }, "203.0.113.7", "203.0.113.7"},
		{"detected replaces stored", func() {
			SetStoredPublicIP("203.0.113.0")
			SetPublicIP("198.51.100.9")
		}, "198.51.100.9", "198.51.100.9"},
		// A backend restarted within the process reloads the stored value.
		{"stored does not replace detected", func() {
			SetPublicIP("198.51.100.9")
			SetStoredPublicIP("203.0.113.0")
		}, "198.51.100.9", "198.51.100.9"},
		// After a network change the old address is kept for geolocation, not sent as exact.
		{"network change forgets detected", func() {
			SetPublicIP("198.51.100.9")
			ForgetDetectedPublicIP()
		}, "198.51.100.9", ""},
		{"lookup from before a network change is discarded", func() {
			network := CurrentNetwork()
			SetStoredPublicIP("203.0.113.0")
			ForgetDetectedPublicIP()
			SetPublicIPOn(network, "198.51.100.9")
		}, "203.0.113.0", ""},
		{"lookup on the current network is sent", func() {
			SetPublicIPOn(CurrentNetwork(), "198.51.100.9")
		}, "198.51.100.9", "198.51.100.9"},
		{"detection after a network change is sent", func() {
			SetPublicIP("198.51.100.9")
			ForgetDetectedPublicIP()
			SetPublicIP("192.0.2.4")
		}, "192.0.2.4", "192.0.2.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetPublicIP("") // each case starts with nothing detected
			tt.set()
			req, err := NewRequestWithHeaders(context.Background(), http.MethodGet, "https://example.com", nil)
			require.NoError(t, err)
			assert.Equal(t, tt.wantAll, req.Header.Get(ClientIPHeader))

			SetDetectedClientIP(req)
			assert.Equal(t, tt.wantExact, req.Header.Get(ClientIPHeader))
			_, present := req.Header[http.CanonicalHeaderKey(ClientIPHeader)]
			assert.Equal(t, tt.wantExact != "", present)
		})
	}
}
