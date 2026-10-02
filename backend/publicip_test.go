package backend

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/publicip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
)

func recordedPublicIP(t *testing.T) string {
	t.Helper()
	req, err := common.NewRequestWithHeaders(context.Background(), http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)
	return req.Header.Get(common.ClientIPHeader)
}

func fixedLookup(ip string, calls *atomic.Int32) func(context.Context) (*publicip.DetectResult, error) {
	return func(context.Context) (*publicip.DetectResult, error) {
		calls.Add(1)
		return &publicip.DetectResult{IP: net.ParseIP(ip)}, nil
	}
}

func runDetect(t *testing.T, direct func() bool, lookup func(context.Context) (*publicip.DetectResult, error)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		detectPublicIP(context.Background(), direct, lookup)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("detectPublicIP did not return")
	}
}

func TestDetectPublicIPRecordsDirectLookup(t *testing.T) {
	common.SetPublicIP("")
	var calls atomic.Int32
	runDetect(t, func() bool { return true }, fixedLookup("203.0.113.7", &calls))
	assert.Equal(t, "203.0.113.7", recordedPublicIP(t))
	assert.EqualValues(t, 1, calls.Load())
}

func TestDetectPublicIPSkipsWhileTunnelUp(t *testing.T) {
	common.SetPublicIP("")
	var calls atomic.Int32
	runDetect(t, func() bool { return false }, fixedLookup("203.0.113.7", &calls))
	assert.Empty(t, recordedPublicIP(t))
	assert.Zero(t, calls.Load(), "no lookup while the tunnel may route it")
}

func TestDetectPublicIPDiscardsResultIfTunnelCameUp(t *testing.T) {
	common.SetPublicIP("")
	var tunnelUp atomic.Bool
	lookup := func(context.Context) (*publicip.DetectResult, error) {
		tunnelUp.Store(true) // the VPN connects while the lookup is in flight
		return &publicip.DetectResult{IP: net.ParseIP("198.51.100.9")}, nil
	}
	runDetect(t, func() bool { return !tunnelUp.Load() }, lookup)
	assert.Empty(t, recordedPublicIP(t), "a result that may be the VPN exit must not be recorded")
}
