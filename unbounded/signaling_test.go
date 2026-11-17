package unbounded

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errSOCKSRejected is what the bypass proxy returned for every signaling
// request while sing-box's direct outbound couldn't resolve freddie.
var errSOCKSRejected = errors.New("socks5: request rejected, code=1")

// recordingRT answers each request with status, recording what it was sent.
type recordingRT struct {
	mu      sync.Mutex
	status  int
	err     error
	bodies  []string
	accepts []string
}

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		body = string(b)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
	r.accepts = append(r.accepts, req.Header.Get("Accept"))
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{StatusCode: r.status, Body: http.NoBody, Request: req}, nil
}

func (r *recordingRT) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// failingDirect is an http.Transport whose dials fail as the bypass proxy's
// did, so it never gets a connection.
func failingDirect() *http.Transport {
	return &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errSOCKSRejected
	}}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestFallback(direct http.RoundTripper, fallback http.RoundTripper) (*fallbackTransport, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	ft := newFallbackTransport(direct, fallback, time.Minute)
	ft.now = c.now
	return ft, c
}

func postGenesis(t *testing.T, rt http.RoundTripper, u string) (*http.Response, error) {
	t.Helper()
	client := &http.Client{Transport: rt}
	return client.PostForm(u, url.Values{"data": {"genesis"}, "room-ticket": {"t"}})
}

func TestSignalingFallsBackWhenDirectCantConnect(t *testing.T) {
	fallback := &recordingRT{status: http.StatusOK}
	ft, _ := newTestFallback(failingDirect(), fallback)

	resp, err := postGenesis(t, ft, "https://freddie.invalid/v1/signal")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, fallback.calls())
	assert.Equal(t, "data=genesis&room-ticket=t", fallback.bodies[0], "the fallback gets the whole form")
	assert.Equal(t, "text/event-stream", fallback.accepts[0], "kindling keeps the long-poll off buffering transports")
}

func TestSignalingDoesNotFallBackOnFreddieErrors(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			freddie := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer freddie.Close()
			fallback := &recordingRT{status: http.StatusOK}
			ft, _ := newTestFallback(freddie.Client().Transport, fallback)

			for range 2 {
				resp, err := postGenesis(t, ft, freddie.URL+"/v1/signal")
				require.NoError(t, err)
				resp.Body.Close()
				assert.Equal(t, code, resp.StatusCode)
			}
			assert.Zero(t, fallback.calls())
			assert.False(t, ft.sticking())
		})
	}
}

// A request that reached freddie may have been acted on, so an error after
// the connection is returned rather than resent.
func TestSignalingDoesNotResendAfterConnecting(t *testing.T) {
	freddie := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		require.NoError(t, err)
		conn.Close()
	}))
	defer freddie.Close()
	fallback := &recordingRT{status: http.StatusOK}
	ft, _ := newTestFallback(freddie.Client().Transport, fallback)

	_, err := postGenesis(t, ft, freddie.URL+"/v1/signal")
	require.Error(t, err)
	assert.Zero(t, fallback.calls())
	assert.False(t, ft.sticking())
}

func TestSignalingSticksToFallbackThenRetriesDirect(t *testing.T) {
	direct := &recordingRT{err: errSOCKSRejected}
	fallback := &recordingRT{status: http.StatusOK}
	ft, clk := newTestFallback(direct, fallback)

	send := func() {
		t.Helper()
		resp, err := postGenesis(t, ft, "https://freddie.invalid/v1/signal")
		require.NoError(t, err)
		resp.Body.Close()
	}

	send()
	require.Equal(t, 1, direct.calls())
	require.Equal(t, 1, fallback.calls())

	// Within the period, requests skip the broken direct path.
	clk.advance(59 * time.Second)
	send()
	send()
	assert.Equal(t, 1, direct.calls())
	assert.Equal(t, 3, fallback.calls())

	// After it, the next request tries direct again, and a failure starts a
	// new period.
	clk.advance(2 * time.Second)
	send()
	assert.Equal(t, 2, direct.calls())
	assert.Equal(t, 4, fallback.calls())
	clk.advance(30 * time.Second)
	send()
	assert.Equal(t, 2, direct.calls(), "the failure restarted the period")

	// Once direct connects again, it carries everything.
	direct.mu.Lock()
	direct.err, direct.status = nil, http.StatusOK
	direct.mu.Unlock()
	clk.advance(time.Minute)
	send()
	send()
	assert.Equal(t, 4, direct.calls())
	assert.Equal(t, 5, fallback.calls())
	assert.Equal(t, "", direct.accepts[3], "direct requests are sent as broflake made them")
}

// The volunteer socket's handshake can't travel over kindling, so it stays
// direct, and broflake POSTs instead when it fails.
func TestSignalingKeepsWebSocketUpgradesDirect(t *testing.T) {
	direct := &recordingRT{err: errSOCKSRejected}
	fallback := &recordingRT{status: http.StatusOK}
	ft, _ := newTestFallback(direct, fallback)

	upgrade := func() error {
		req, err := http.NewRequest(http.MethodGet, "https://freddie.invalid/v1/volunteer", nil)
		require.NoError(t, err)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		_, err = ft.RoundTrip(req)
		return err
	}

	require.ErrorIs(t, upgrade(), errSOCKSRejected)
	assert.Zero(t, fallback.calls())
	assert.True(t, ft.sticking(), "a failed handshake moves the POSTs to the fallback")

	require.ErrorIs(t, upgrade(), errSOCKSRejected)
	assert.Equal(t, 2, direct.calls(), "upgrades go direct even while the POSTs don't")
	assert.Zero(t, fallback.calls())
}

func TestSignalingDoesNotFallBackWhenCanceled(t *testing.T) {
	fallback := &recordingRT{status: http.StatusOK}
	ft, _ := newTestFallback(failingDirect(), fallback)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://freddie.invalid/v1/signal", strings.NewReader("x"))
	require.NoError(t, err)
	_, err = ft.RoundTrip(req)
	require.Error(t, err)
	assert.Zero(t, fallback.calls())
	assert.False(t, ft.sticking())
}

func TestSignalingCantResendWithoutGetBody(t *testing.T) {
	fallback := &recordingRT{status: http.StatusOK}
	ft, _ := newTestFallback(failingDirect(), fallback)
	req, err := http.NewRequest(http.MethodPost, "https://freddie.invalid/v1/signal", io.NopCloser(strings.NewReader("x")))
	require.NoError(t, err)
	require.Nil(t, req.GetBody)
	_, err = ft.RoundTrip(req)
	require.ErrorIs(t, err, errSOCKSRejected)
	assert.Zero(t, fallback.calls())
	assert.True(t, ft.sticking(), "the next request still skips direct")
}

// The volunteer socket dials through the signaling client, so a handshake
// that goes direct must come back usable.
func TestSignalingWebSocketHandshakeStillWorks(t *testing.T) {
	freddie := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, msg)
	}))
	defer freddie.Close()
	fallback := &recordingRT{status: http.StatusOK}
	ft, _ := newTestFallback(freddie.Client().Transport, fallback)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(freddie.URL, "http")+"/v1/volunteer",
		&websocket.DialOptions{HTTPClient: &http.Client{Transport: ft}})
	require.NoError(t, err)
	defer c.CloseNow()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte("offer")))
	_, msg, err := c.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, "offer", string(msg))
	assert.Zero(t, fallback.calls())
}
