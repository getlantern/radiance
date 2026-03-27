package vpn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLazyDirectTransport_UnresolvedRoundTrip(t *testing.T) {
	transport := &lazyDirectTransport{}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	_, err := transport.RoundTrip(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet resolved")
}

func TestLazyDirectTransport_ResolvedRoundTrip(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	transport := &lazyDirectTransport{}

	// Manually resolve with a working transport (simulates what Resolve does)
	transport.mu.Lock()
	transport.inner = http.DefaultTransport
	transport.resolved = true
	transport.mu.Unlock()

	req, _ := http.NewRequest("GET", ts.URL, nil)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestLazyDirectTransport_ResolveNoOutboundManager(t *testing.T) {
	transport := &lazyDirectTransport{}
	err := transport.Resolve(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outbound manager not found")
}

func TestLazyDirectTransport_ConcurrentAccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	transport := &lazyDirectTransport{}
	transport.mu.Lock()
	transport.inner = http.DefaultTransport
	transport.resolved = true
	transport.mu.Unlock()

	// Fire several concurrent requests to verify no data race
	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			req, _ := http.NewRequest("GET", ts.URL, nil)
			resp, err := transport.RoundTrip(req)
			assert.NoError(t, err)
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}
