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

// autoSelectedTunnel reports a fixed auto selection; its other methods are
// the nil VPNClient's and must not be called.
type autoSelectedTunnel struct {
	*vpn.VPNClient
	tag string
}

func (a autoSelectedTunnel) CurrentAutoSelectedServer() (string, error) { return a.tag, nil }

func TestAutoSelectedEventsStreamSendsCurrentOnSubscribe(t *testing.T) {
	subscribe := func(t *testing.T, current string) *bufio.Reader {
		b := &backend.LocalBackend{}
		b.SetLiveTunnelForTest(autoSelectedTunnel{tag: current})
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

	t.Run("current selection", func(t *testing.T) {
		body := subscribe(t, "fastest")
		require.Equal(t, "fastest", readAutoSelected(t, body))
		events.Emit(vpn.AutoSelectedEvent{Selected: "next"})
		require.Equal(t, "next", readAutoSelected(t, body))
	})

	t.Run("no selection", func(t *testing.T) {
		// Emit until the handler has subscribed, which can be after the
		// response headers arrive. The first frame must be an emitted one,
		// not a snapshot.
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				events.Emit(vpn.AutoSelectedEvent{Selected: "next"})
				select {
				case <-stop:
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()
		t.Cleanup(func() {
			close(stop)
			<-done
		})
		body := subscribe(t, "")
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
		streamFor time.Duration
		wantReset bool
	}{
		{name: "stable stream", streamFor: 2 * sseStableAfter, wantReset: true},
		{name: "immediately closed stream", streamFor: 0, wantReset: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var attempts []time.Time
				c := newClient()
				c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					attempts = append(attempts, time.Now())
					switch n := len(attempts); {
					case n <= failures:
						return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
					case n == failures+1:
						return &http.Response{
							StatusCode: http.StatusOK,
							Body:       io.NopCloser(eofAfter(tt.streamFor)),
							Header:     make(http.Header),
						}, nil
					default:
						cancel()
						return nil, context.Canceled
					}
				})

				require.ErrorIs(t, c.sseRetryLoop(ctx, "/test/events", func([]byte) {}), context.Canceled)
				require.Len(t, attempts, failures+2)

				wait := attempts[failures+1].Sub(attempts[failures]) - tt.streamFor
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
