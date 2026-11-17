package unbounded

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/radiance/kindling"
)

// signalingFallbackFor is how long the widget's signaling stays on the
// fallback after the direct path fails to connect, so a broken direct path
// costs one failed dial per period rather than one per request.
const signalingFallbackFor = 90 * time.Second

// signalingTransport carries the widget's signaling with freddie. It prefers
// the direct path around the tunnel, and falls back to kindling, which the
// Unbounded consumer already signals through, when that path can't connect.
// After a Wi-Fi address change sing-box's direct outbound failed every lookup
// for minutes, so a volunteer couldn't offer itself to anyone while its own
// consumer side still reached freddie through kindling.
var signalingTransport http.RoundTripper = newFallbackTransport(
	bypassHTTPClient.Transport, kindling.HTTPClient().Transport, signalingFallbackFor)

// fallbackTransport sends each request direct unless a recent direct request
// failed to get a connection, and retries a request that failed that way on
// the fallback. A request that got a connection is never retried: freddie may
// have acted on it, and an HTTP error status is freddie's answer, not a sign
// the path is broken.
//
// A WebSocket upgrade only ever goes direct. kindling's transports can't
// carry one, and broflake falls back to POSTs when its socket won't open.
type fallbackTransport struct {
	direct   http.RoundTripper
	fallback http.RoundTripper
	stickFor time.Duration
	now      func() time.Time

	mu sync.Mutex
	// until is when requests go direct again.
	until time.Time
	// fellBack is whether a request has used the fallback since the last
	// direct request that connected.
	fellBack bool
}

func newFallbackTransport(direct, fallback http.RoundTripper, stickFor time.Duration) *fallbackTransport {
	return &fallbackTransport{direct: direct, fallback: fallback, stickFor: stickFor, now: time.Now}
}

func (t *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	upgrade := isUpgrade(req)
	if !upgrade && t.sticking() {
		return t.fallback.RoundTrip(streaming(req.Clone(req.Context())))
	}
	resp, connected, err := t.roundTripDirect(req)
	if err == nil || connected {
		t.directConnected()
		return resp, err
	}
	if req.Context().Err() != nil {
		return nil, err
	}
	t.stick(err)
	if upgrade {
		return nil, err
	}
	retry, rerr := rewind(req)
	if rerr != nil {
		return nil, errors.Join(err, rerr)
	}
	return t.fallback.RoundTrip(streaming(retry))
}

// roundTripDirect sends req direct, reporting whether it got a connection.
// One that didn't never left this device.
func (t *fallbackTransport) roundTripDirect(req *http.Request) (*http.Response, bool, error) {
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	resp, err := t.direct.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	return resp, connected.Load(), err
}

func (t *fallbackTransport) sticking() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now().Before(t.until)
}

func (t *fallbackTransport) stick(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.fellBack {
		slog.Info("Unbounded: direct signaling can't connect, signaling through kindling",
			"for", t.stickFor, "error", err)
	}
	t.fellBack = true
	t.until = t.now().Add(t.stickFor)
}

func (t *fallbackTransport) directConnected() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fellBack {
		slog.Info("Unbounded: direct signaling connects again")
	}
	t.fellBack = false
	t.until = time.Time{}
}

func isUpgrade(req *http.Request) bool {
	for _, v := range req.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return req.Header.Get("Upgrade") != ""
}

// rewind returns a copy of req with a fresh body, since the failed direct
// attempt closed the original.
func rewind(req *http.Request) (*http.Request, error) {
	out := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return out, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("unbounded: can't resend a signaling request without GetBody")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	out.Body = body
	return out, nil
}

// streaming marks a request that the caller owns as streaming, so kindling
// leaves out transports that would buffer freddie's long-polled replies, as
// the consumer's signaling does.
func streaming(req *http.Request) *http.Request {
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req
}
