package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/vpn"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// eofAfter is a stream body that ends after the given duration.
type eofAfter time.Duration

func (d eofAfter) Read([]byte) (int, error) {
	time.Sleep(time.Duration(d))
	return 0, io.EOF
}

// autoSelectedTunnel reports a settable auto selection and signals each read;
// its other methods are the nil VPNClient's and must not be called.
type autoSelectedTunnel struct {
	*vpn.VPNClient
	mu    sync.Mutex
	tag   string
	reads chan struct{}
}

func newAutoSelectedTunnel(tag string) *autoSelectedTunnel {
	return &autoSelectedTunnel{tag: tag, reads: make(chan struct{}, 1)}
}

func (a *autoSelectedTunnel) set(tag string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tag = tag
}

func (a *autoSelectedTunnel) CurrentAutoSelectedServer() (string, error) {
	a.mu.Lock()
	tag := a.tag
	a.mu.Unlock()
	select {
	case a.reads <- struct{}{}:
	default:
	}
	return tag, nil
}

func TestAutoSelectedEventsStream(t *testing.T) {
	subscribe := func(t *testing.T, tun *autoSelectedTunnel) *bufio.Reader {
		b := &backend.LocalBackend{}
		b.SetLiveTunnelForTest(tun)
		srv := httptest.NewServer(newLocalAPI(b, false))
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+serverAutoSelectedEventsEndpoint, nil)
		require.NoError(t, err)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return bufio.NewReader(resp.Body)
	}

	t.Run("sends the current selection on subscribe and on each event", func(t *testing.T) {
		tun := newAutoSelectedTunnel("fastest")
		body := subscribe(t, tun)
		require.Equal(t, "fastest", readAutoSelected(t, body))

		tun.set("next")
		events.Emit(vpn.AutoSelectedEvent{Selected: "stale"})
		require.Equal(t, "next", readAutoSelected(t, body))
	})

	t.Run("sends nothing without a selection", func(t *testing.T) {
		tun := newAutoSelectedTunnel("")
		body := subscribe(t, tun)
		// The first read happens after the handler subscribes.
		<-tun.reads

		tun.set("next")
		events.Emit(vpn.AutoSelectedEvent{Selected: "next"})
		require.Equal(t, "next", readAutoSelected(t, body))
	})
}

func readAutoSelected(t *testing.T, body *bufio.Reader) string {
	t.Helper()
	line, err := body.ReadString('\n')
	require.NoError(t, err)
	data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
	require.True(t, ok, line)
	var evt vpn.AutoSelectedEvent
	require.NoError(t, json.Unmarshal([]byte(data), &evt))
	_, err = body.ReadString('\n')
	require.NoError(t, err)
	return evt.Selected
}

func TestSSERetryLoopResetsBackoffAfterStableStream(t *testing.T) {
	const failures = 20
	tests := []struct {
		name      string
		accept    bool
		lasts     time.Duration
		wantReset bool
	}{
		{name: "stable stream", accept: true, lasts: 2 * sseStableAfter, wantReset: true},
		{name: "immediately closed stream", accept: true, lasts: 0, wantReset: false},
		{name: "slow failed connect", accept: false, lasts: 2 * sseStableAfter, wantReset: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				refused := &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
				var attempts []time.Time
				c := newClient()
				c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					attempts = append(attempts, time.Now())
					switch n := len(attempts); {
					case n <= failures:
						return nil, refused
					case n == failures+1 && tt.accept:
						return &http.Response{
							StatusCode: http.StatusOK,
							Body:       io.NopCloser(eofAfter(tt.lasts)),
							Header:     make(http.Header),
						}, nil
					case n == failures+1:
						time.Sleep(tt.lasts)
						return nil, refused
					default:
						cancel()
						return nil, context.Canceled
					}
				})

				require.ErrorIs(t, c.sseRetryLoop(ctx, "/test/events", func([]byte) {}), context.Canceled)
				require.Len(t, attempts, failures+2)

				wait := attempts[failures+1].Sub(attempts[failures]) - tt.lasts
				if tt.wantReset {
					// First backoff step: 10ms with ±20% jitter.
					require.LessOrEqual(t, wait, 12*time.Millisecond)
				} else {
					// Step failures+1 without a reset: 10ms × 21² with -20% jitter.
					require.GreaterOrEqual(t, wait, 3*time.Second)
				}
			})
		})
	}
}
