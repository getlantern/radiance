package peer

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// portBoxes models how production boxes share the peer's one listening port:
// building a samizdat box binds it, and a second build fails with EADDRINUSE
// until the first box is closed. The fake box in peer_test.go binds nothing,
// which is how a rotation that built before closing went unnoticed.
type portBoxes struct {
	bound    atomic.Bool
	failFrom atomic.Int64 // builds numbered >= this fail outright; 0 means never

	mu    sync.Mutex
	boxes []*portBox
}

type portBox struct {
	*fakeBoxService
	port *portBoxes
}

func (b *portBox) Close() error {
	b.port.bound.Store(false)
	return b.fakeBoxService.Close()
}

func (p *portBoxes) build(_ context.Context, options string) (boxService, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.failFrom.Load(); n > 0 && int64(len(p.boxes))+1 >= n {
		return nil, errors.New("build sing-box: initialize inbound[0]: bad launch config")
	}
	if !p.bound.CompareAndSwap(false, true) {
		return nil, errors.New("build sing-box: initialize inbound[0]: creating TCP listener: listen tcp 0.0.0.0:5698: bind: address already in use")
	}
	b := &portBox{fakeBoxService: &fakeBoxService{gotConfig: options}, port: p}
	p.boxes = append(p.boxes, b)
	return b, nil
}

func (p *portBoxes) built() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.boxes)
}

// The server upserts a peer's route on (address, port), so re-registering
// from the same peer returns the same route_id. Rotation must rebind the port
// and must not deregister that route_id, which is the live one: doing so is
// what stopped a production peer an hour after it started.
func TestClient_RotationWithSameRouteIDKeepsServing(t *testing.T) {
	srv := newStubServer(t) // every register returns the same route_id
	port := &portBoxes{}
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.CredRotationInterval = 50 * time.Millisecond
		cfg.HeartbeatInterval = time.Hour
		cfg.BuildBoxService = port.build
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	require.Eventually(t, func() bool { return port.built() >= 3 }, 2*time.Second, 10*time.Millisecond,
		"each rotation should rebuild the box on the same port (built %d)", port.built())
	assert.True(t, c.IsActive(), "rotation must leave the peer serving")
	assert.Zero(t, srv.deregisterCount.Load(), "rotation must not deregister the live route_id")
	assert.Equal(t, srv.registerResp.RouteID, c.CurrentStatus().RouteID)

	port.mu.Lock()
	defer port.mu.Unlock()
	for i, b := range port.boxes[:len(port.boxes)-1] {
		assert.True(t, b.closed.Load(), "box %d should be closed by the rotation that replaced it", i)
	}
}

// When a rotation on the same route_id fails, the server is already handing
// out the new credentials and nothing serves them, so the client must stop
// itself and say so, not keep heartbeating a route that can't be used.
func TestClient_FailedRotationOnSameRouteStopsClient(t *testing.T) {
	srv := newStubServer(t)
	port := &portBoxes{}
	port.failFrom.Store(2) // the initial build succeeds, every rebuild fails
	selfStopped := make(chan error, 1)
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.CredRotationInterval = 50 * time.Millisecond
		cfg.HeartbeatInterval = time.Hour
		cfg.BuildBoxService = port.build
		cfg.OnSelfStop = func(reason error) { selfStopped <- reason }
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	select {
	case reason := <-selfStopped:
		assert.ErrorContains(t, reason, "credential rotation")
	case <-time.After(3 * time.Second):
		t.Fatal("a failed same-route rotation should stop the client")
	}
	assert.False(t, c.IsActive())
}

// A rotation that fails before touching the old box, on a new route_id, keeps
// the old box serving and only cleans up the route it just created.
func TestClient_FailedRotationOnNewRouteKeepsOldBox(t *testing.T) {
	srv := newStubServer(t)
	var seq atomic.Int64
	srv.registerRespFn = func() RegisterResponse {
		if seq.Add(1) == 1 {
			return srv.registerResp
		}
		return RegisterResponse{RouteID: "00000000-0000-0000-0000-000000000999", ServerConfig: `{"route":{}}`, HeartbeatIntervalSeconds: 60}
	}
	var selfStops atomic.Int64
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.CredRotationInterval = 50 * time.Millisecond
		cfg.HeartbeatInterval = time.Hour
		cfg.OnSelfStop = func(error) { selfStops.Add(1) }
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	require.Eventually(t, func() bool { return srv.deregisterCount.Load() >= 1 }, 2*time.Second, 10*time.Millisecond,
		"the rejected new route should be deregistered")
	assert.True(t, c.IsActive(), "the old box still serves the old route")
	assert.Equal(t, srv.registerResp.RouteID, c.CurrentStatus().RouteID)
	assert.Zero(t, selfStops.Load())
}

// A heartbeat 404 means the server dropped the route; the client stops and
// reports it, so the backend can turn the toggle off.
func TestClient_HeartbeatNotRegisteredReportsSelfStop(t *testing.T) {
	srv := newStubServer(t)
	srv.heartbeatStatus = http.StatusNotFound
	selfStopped := make(chan error, 1)
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.OnSelfStop = func(reason error) { selfStopped <- reason }
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	select {
	case reason := <-selfStopped:
		assert.ErrorContains(t, reason, "no longer registered")
	case <-time.After(3 * time.Second):
		t.Fatal("a heartbeat 404 should stop the client and report it")
	}
	assert.False(t, c.IsActive())
}

// samizdat's Close releases the port but then waits for every client
// connection to end, which took 18 minutes in production. Stop must not.
func TestClient_StopDoesNotWaitForDrainingClose(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	box := &drainingBox{fakeBoxService: &fakeBoxService{}, release: release}
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, newStubServer(t), func(cfg *Config) {
		cfg.HeartbeatInterval = time.Hour
		cfg.BoxCloseTimeout = 50 * time.Millisecond
		cfg.BuildBoxService = func(context.Context, string) (boxService, error) { return box, nil }
	})
	require.NoError(t, c.Start(context.Background()))

	start := time.Now()
	require.NoError(t, c.Stop(context.Background()))
	assert.Less(t, time.Since(start), time.Second, "Stop should give up on a draining Close")
	assert.False(t, c.IsActive())
}

type drainingBox struct {
	*fakeBoxService
	release chan struct{}
}

func (b *drainingBox) Close() error {
	<-b.release
	return b.fakeBoxService.Close()
}

// A self-stop decided by one session must not stop the session a user
// started after it.
func TestClient_StaleSelfStopSparesReplacementSession(t *testing.T) {
	var selfStops atomic.Int64
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, newStubServer(t), func(cfg *Config) {
		cfg.HeartbeatInterval = time.Hour
		cfg.OnSelfStop = func(error) { selfStops.Add(1) }
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	c.mu.Lock()
	oldSession := c.runCtx
	c.mu.Unlock()
	require.NoError(t, c.Stop(ctx))
	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() { _ = c.Stop(ctx) })

	c.stopSelf(oldSession, errors.New("old session's rotation failed"))
	time.Sleep(100 * time.Millisecond)
	assert.True(t, c.IsActive(), "the replacement session keeps running")
	assert.Zero(t, selfStops.Load())
}

// The server reuses the route_id across re-registrations, so a 404 for the
// registration a rotation just replaced must not stop the peer.
func TestClient_HeartbeatNotFoundForReplacedRegistrationIsIgnored(t *testing.T) {
	srv := newStubServer(t)
	srv.heartbeatStatus = http.StatusNotFound
	var selfStops atomic.Int64
	var c *Client
	var calls atomic.Int64
	srv.onHeartbeat = func() {
		switch calls.Add(1) {
		case 1:
			// A rotation on the same route_id completes while this heartbeat
			// is in flight; the heartbeat still gets its 404.
			c.mu.Lock()
			c.registration++
			c.mu.Unlock()
		case 2:
			srv.heartbeatStatus = http.StatusOK
		}
	}
	c = newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.OnSelfStop = func(error) { selfStops.Add(1) }
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	require.Eventually(t, func() bool { return srv.heartbeatCount.Load() >= 3 }, 2*time.Second, 10*time.Millisecond)
	assert.True(t, c.IsActive())
	assert.Zero(t, selfStops.Load())
}

// A box that was built, and so holds the port, but panicked or failed on
// Start is closed before giving up, with the bounded close.
func TestBuildAndStartWithRetry_ClosesBoxThatFailedToStart(t *testing.T) {
	var closed []*fakeBoxService
	closeBox := func(b boxService) error {
		closed = append(closed, b.(*panicBox).fakeBoxService)
		return nil
	}
	panicking := &panicBox{fakeBoxService: &fakeBoxService{}, panicOnStart: true}
	_, err := buildAndStartWithRetry(context.Background(), func() (boxService, error) { return panicking, nil }, closeBox)
	require.ErrorContains(t, err, "panicked")
	require.Len(t, closed, 1)

	closed = nil
	failing := &panicBox{fakeBoxService: &fakeBoxService{startErr: errors.New("start failed")}}
	_, err = buildAndStartWithRetry(context.Background(), func() (boxService, error) { return failing, nil }, closeBox)
	require.ErrorContains(t, err, "start failed")
	assert.Len(t, closed, 5, "every failed attempt releases its box")
}

type panicBox struct {
	*fakeBoxService
	panicOnStart bool
}

func (b *panicBox) Start() error {
	if b.panicOnStart {
		panic("libbox start")
	}
	return b.fakeBoxService.Start()
}
