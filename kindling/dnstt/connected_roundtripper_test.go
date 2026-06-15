package dnstt

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tunnel that keeps returning a gateway 5xx must be demoted rather than
// re-marked healthy on every response, so a persistently-broken tunnel leaves
// the pool. Origin-generated statuses (4xx, 500) leave the tunnel healthy.
func TestConnectedRoundtripperHealthTracking(t *testing.T) {
	newCRT := func(status int) (*connectedRoundtripper, *dnsTunnel) {
		tun := &dnsTunnel{domain: "t.iantem.io", resolver: "https://cloudflare-dns.com/dns-query"}
		tun.markSucceeded() // a probed tunnel starts healthy
		rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: http.NoBody, Request: req}, nil
		})
		return &connectedRoundtripper{t: tun, rt: rt}, tun
	}
	req := httptest.NewRequest(http.MethodGet, "https://api.getiantem.org/v1/x", nil)

	// roundTrip issues one request and closes the body, per the
	// http.RoundTripper contract.
	roundTrip := func(t *testing.T, crt *connectedRoundtripper) *http.Response {
		t.Helper()
		resp, err := crt.RoundTrip(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp
	}

	t.Run("gateway 5xx demotes the tunnel", func(t *testing.T) {
		for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
			crt, tun := newCRT(status)
			for range maxTunnelFailures {
				// the 5xx is still surfaced, not converted to an error
				assert.Equal(t, status, roundTrip(t, crt).StatusCode)
			}
			assert.False(t, tun.isSucceeding(), "tunnel returning %d should be demoted", status)
		}
	})

	t.Run("2xx keeps the tunnel healthy", func(t *testing.T) {
		crt, tun := newCRT(http.StatusOK)
		for range maxTunnelFailures + 2 {
			roundTrip(t, crt)
		}
		assert.True(t, tun.isSucceeding())
	})

	t.Run("origin 4xx/500 does not demote the tunnel", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
			crt, tun := newCRT(status)
			for range maxTunnelFailures + 2 {
				roundTrip(t, crt)
			}
			assert.True(t, tun.isSucceeding(), "status %d is an origin verdict, tunnel stays healthy", status)
		}
	})
}
