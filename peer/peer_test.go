package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/portforward"
)

type fakeForwarder struct {
	mu          sync.Mutex
	mapErr      error
	extIPErr    error
	unmapErr    error
	mapped      bool
	unmapped    bool
	renewals    int
	externalIP  string
	mapping     *portforward.Mapping
	cancelRenew context.CancelFunc
}

func (f *fakeForwarder) MapPort(_ context.Context, internalPort uint16, _ string) (*portforward.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mapErr != nil {
		return nil, f.mapErr
	}
	f.mapped = true
	f.mapping = &portforward.Mapping{
		ExternalPort:  internalPort,
		InternalPort:  internalPort,
		InternalIP:    "192.168.1.10",
		Protocol:      "TCP",
		LeaseDuration: time.Hour,
		Method:        "fake",
	}
	return f.mapping, nil
}

func (f *fakeForwarder) UnmapPort(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmapped = true
	return f.unmapErr
}

func (f *fakeForwarder) StartRenewal(ctx context.Context) {
	f.mu.Lock()
	f.renewals++
	rctx, cancel := context.WithCancel(ctx)
	f.cancelRenew = cancel
	f.mu.Unlock()
	go func() { <-rctx.Done() }()
}

func (f *fakeForwarder) ExternalIP(_ context.Context) (string, error) {
	if f.extIPErr != nil {
		return "", f.extIPErr
	}
	if f.externalIP == "" {
		return "203.0.113.99", nil
	}
	return f.externalIP, nil
}

func (f *fakeForwarder) wasUnmapped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unmapped
}

func (f *fakeForwarder) wasMapped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mapped
}

// slowMapForwarder blocks MapPort on a gate channel and signals via entered
// when the call is in flight. Used to race two concurrent Starts so the
// test can observe the serialization invariant.
type slowMapForwarder struct {
	gate    chan struct{}
	entered chan struct{}
}

func (f *slowMapForwarder) MapPort(_ context.Context, internalPort uint16, _ string) (*portforward.Mapping, error) {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-f.gate
	return &portforward.Mapping{
		ExternalPort: internalPort, InternalPort: internalPort,
		InternalIP: "192.168.1.10", Protocol: "TCP",
		LeaseDuration: time.Hour, Method: "fake",
	}, nil
}
func (f *slowMapForwarder) UnmapPort(context.Context) error { return nil }
func (f *slowMapForwarder) StartRenewal(context.Context)    {}
func (f *slowMapForwarder) ExternalIP(context.Context) (string, error) {
	return "203.0.113.99", nil
}

type fakeBoxService struct {
	startErr  error
	closeErr  error
	started   atomic.Bool
	closed    atomic.Bool
	gotConfig string
}

func (b *fakeBoxService) Start() error {
	if b.startErr != nil {
		return b.startErr
	}
	b.started.Store(true)
	return nil
}

func (b *fakeBoxService) Close() error {
	b.closed.Store(true)
	return b.closeErr
}

type stubServer struct {
	t                  *testing.T
	server             *httptest.Server
	registerStatus     int
	registerResp       RegisterResponse
	heartbeatStatus    int
	deregisterStatus   int
	registerCount      atomic.Int64
	heartbeatCount     atomic.Int64
	deregisterCount    atomic.Int64
	registerDeviceID   atomic.Value // string
	heartbeatDeviceID  atomic.Value // string
	deregisterDeviceID atomic.Value // string
	lastRegisterReq    atomic.Value // RegisterRequest
}

func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{
		t:                t,
		registerStatus:   http.StatusOK,
		heartbeatStatus:  http.StatusOK,
		deregisterStatus: http.StatusOK,
		registerResp: RegisterResponse{
			RouteID:                  "00000000-0000-0000-0000-000000000123",
			ServerConfig:             `{"inbounds": [{"type":"samizdat","tag":"samizdat-in"}]}`,
			HeartbeatIntervalSeconds: 60,
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/peer/register", func(w http.ResponseWriter, r *http.Request) {
		s.registerCount.Add(1)
		s.registerDeviceID.Store(r.Header.Get("X-Lantern-Device-Id"))
		var req RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.lastRegisterReq.Store(req)
		if s.registerStatus != http.StatusOK {
			http.Error(w, "register failed", s.registerStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(s.registerResp)
	})
	mux.HandleFunc("/v1/peer/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		s.heartbeatCount.Add(1)
		s.heartbeatDeviceID.Store(r.Header.Get("X-Lantern-Device-Id"))
		if s.heartbeatStatus != http.StatusOK {
			http.Error(w, "heartbeat failed", s.heartbeatStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/peer/deregister", func(w http.ResponseWriter, r *http.Request) {
		s.deregisterCount.Add(1)
		s.deregisterDeviceID.Store(r.Header.Get("X-Lantern-Device-Id"))
		if s.deregisterStatus != http.StatusOK {
			http.Error(w, "deregister failed", s.deregisterStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

// newTestClient builds a Client wired to the supplied test doubles. The
// HeartbeatInterval default of 0 leaves the production floor in place
// (caller can override per test).
func newTestClient(t *testing.T, fwd portForwarder, box *fakeBoxService, srv *stubServer, opts ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		API: NewAPI(srv.server.Client(), srv.server.URL, "test-device"),
		NewForwarder: func(_ context.Context) (portForwarder, error) {
			return fwd, nil
		},
		BuildBoxService: func(_ context.Context, options string) (boxService, error) {
			box.gotConfig = options
			return box, nil
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := NewClient(cfg)
	require.NoError(t, err)
	return c
}

func TestClient_Start_HappyPath(t *testing.T) {
	fwd := &fakeForwarder{externalIP: "203.0.113.42"}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() { _ = c.Stop(ctx) })

	assert.True(t, c.IsActive())
	assert.True(t, fwd.wasMapped())
	assert.True(t, box.started.Load())
	assert.Equal(t, int64(1), srv.registerCount.Load())
	assert.Equal(t, "test-device", srv.registerDeviceID.Load())

	req := srv.lastRegisterReq.Load().(RegisterRequest)
	assert.Equal(t, "203.0.113.42", req.ExternalIP)
	assert.NotZero(t, req.ExternalPort)
	assert.NotZero(t, req.InternalPort)

	status := c.CurrentStatus()
	assert.True(t, status.Active)
	assert.Equal(t, "203.0.113.42", status.ExternalIP)
	assert.Equal(t, "00000000-0000-0000-0000-000000000123", status.RouteID)
}

func TestClient_Start_DoubleStartIsError(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	err := c.Start(context.Background())
	assert.ErrorContains(t, err, "already active")
}

// Two goroutines hitting Start at the same time must not both run setup —
// the second one would overwrite the first's state, leaving the first
// session orphaned with no way to Stop it through this Client.
func TestClient_Start_ConcurrentStartsAreSerialized(t *testing.T) {
	fwd := &slowMapForwarder{
		gate:    make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	results := make(chan error, 2)
	for range 2 {
		go func() { results <- c.Start(context.Background()) }()
	}
	// Wait for one Start to be inside MapPort holding starting=true; release
	// it once the second Start has had a chance to observe the contended
	// state and reject.
	<-fwd.entered
	close(fwd.gate)

	var nilCount, errCount int
	for range 2 {
		if err := <-results; err == nil {
			nilCount++
		} else {
			errCount++
			assert.ErrorContains(t, err, "already active")
		}
	}
	assert.Equal(t, 1, nilCount, "exactly one Start must succeed")
	assert.Equal(t, 1, errCount, "the racing Start must be rejected")
	assert.Equal(t, int64(1), srv.registerCount.Load())
}

func TestClient_Start_PortForwardFailureUnwinds(t *testing.T) {
	fwd := &fakeForwarder{mapErr: portforward.ErrNoPortForwarding}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	err := c.Start(context.Background())
	require.Error(t, err)
	assert.False(t, c.IsActive())
	assert.Equal(t, int64(0), srv.registerCount.Load())
	assert.False(t, box.started.Load())
}

func TestClient_Start_ExternalIPFailureUnwinds(t *testing.T) {
	fwd := &fakeForwarder{extIPErr: errors.New("gateway returned empty")}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	err := c.Start(context.Background())
	require.Error(t, err)
	assert.False(t, c.IsActive())
	assert.True(t, fwd.wasUnmapped(), "port must be unmapped after external-ip failure")
	assert.Equal(t, int64(0), srv.registerCount.Load())
	assert.False(t, box.started.Load())
}

func TestClient_Start_RegisterFailureUnwinds(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	srv.registerStatus = http.StatusUnprocessableEntity
	c := newTestClient(t, fwd, box, srv)

	err := c.Start(context.Background())
	require.Error(t, err)
	assert.False(t, c.IsActive())
	assert.True(t, fwd.wasUnmapped())
	assert.False(t, box.started.Load())
}

func TestClient_Start_BoxStartFailureUnwinds(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{startErr: errors.New("boom")}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	err := c.Start(context.Background())
	require.Error(t, err)
	assert.False(t, c.IsActive())
	assert.True(t, fwd.wasUnmapped())
	assert.True(t, box.closed.Load())
	assert.Equal(t, int64(1), srv.deregisterCount.Load())
}

func TestClient_Stop_HappyPath(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	require.NoError(t, c.Stop(ctx))

	assert.False(t, c.IsActive())
	assert.True(t, fwd.wasUnmapped())
	assert.True(t, box.closed.Load())
	assert.Equal(t, int64(1), srv.deregisterCount.Load())
	assert.Equal(t, "test-device", srv.deregisterDeviceID.Load())
}

func TestClient_Stop_IsIdempotent(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	require.NoError(t, c.Stop(ctx))
	require.NoError(t, c.Stop(ctx))
	assert.Equal(t, int64(1), srv.deregisterCount.Load())
}

// Stop continues teardown even if individual steps fail. The first error is
// returned; the others are logged. All resources still get released.
func TestClient_Stop_ContinuesPastIndividualErrors(t *testing.T) {
	fwd := &fakeForwarder{unmapErr: errors.New("router said no")}
	box := &fakeBoxService{closeErr: errors.New("box close failed")}
	srv := newStubServer(t)
	srv.deregisterStatus = http.StatusInternalServerError
	c := newTestClient(t, fwd, box, srv)

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	err := c.Stop(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "deregister")

	assert.False(t, c.IsActive())
	assert.True(t, fwd.wasUnmapped())
	assert.True(t, box.closed.Load())
	assert.Equal(t, int64(1), srv.deregisterCount.Load())
}

// Drives the loop with a 50ms interval (overridden via Config.HeartbeatInterval)
// against a server that always 404s, then waits for the auto-stop goroutine to
// flip IsActive() false and run teardown.
func TestClient_Heartbeat_404TriggersAutoStop(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	srv.heartbeatStatus = http.StatusNotFound
	c := newTestClient(t, fwd, box, srv, func(cfg *Config) {
		cfg.HeartbeatInterval = 50 * time.Millisecond
		cfg.HeartbeatTimeout = 1 * time.Second
	})

	require.NoError(t, c.Start(context.Background()))

	deadline := time.After(3 * time.Second)
	for c.IsActive() {
		select {
		case <-deadline:
			t.Fatal("client did not auto-stop within 3s")
		case <-time.After(20 * time.Millisecond):
		}
	}

	assert.GreaterOrEqual(t, srv.heartbeatCount.Load(), int64(1))
	assert.Equal(t, "test-device", srv.heartbeatDeviceID.Load())
	assert.Equal(t, int64(1), srv.deregisterCount.Load())
	assert.True(t, fwd.wasUnmapped())
	assert.True(t, box.closed.Load())
}

// Non-404 heartbeat errors must not tear the client down — they're logged
// and the loop keeps trying.
func TestClient_Heartbeat_TransientErrorDoesNotStop(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	srv.heartbeatStatus = http.StatusInternalServerError
	c := newTestClient(t, fwd, box, srv, func(cfg *Config) {
		cfg.HeartbeatInterval = 50 * time.Millisecond
		cfg.HeartbeatTimeout = 1 * time.Second
	})

	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	// Wait long enough for several heartbeats to fire.
	deadline := time.After(500 * time.Millisecond)
	for srv.heartbeatCount.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d heartbeats fired in 500ms", srv.heartbeatCount.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	assert.True(t, c.IsActive())
	assert.Equal(t, int64(0), srv.deregisterCount.Load())
}

// The peer's sing-box must bypass the user's own VPN TUN — verify both the
// "no route block at all" and "existing route block" cases get the flag set,
// and that other route-level keys are preserved.
func TestEnsurePeerOutboundsBypassVPN(t *testing.T) {
	t.Run("adds route block when missing", func(t *testing.T) {
		in := `{"inbounds":[{"type":"samizdat","tag":"samizdat-in"}]}`
		out, err := ensurePeerOutboundsBypassVPN(in)
		require.NoError(t, err)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &parsed))
		route := parsed["route"].(map[string]any)
		assert.Equal(t, true, route["auto_detect_interface"])
		assert.Contains(t, parsed, "inbounds", "must preserve other top-level fields")
	})
	t.Run("preserves existing route fields", func(t *testing.T) {
		in := `{"route":{"rules":[{"action":"sniff"}],"final":"direct"}}`
		out, err := ensurePeerOutboundsBypassVPN(in)
		require.NoError(t, err)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &parsed))
		route := parsed["route"].(map[string]any)
		assert.Equal(t, true, route["auto_detect_interface"])
		assert.Equal(t, "direct", route["final"])
		assert.NotEmpty(t, route["rules"])
	})
	t.Run("rejects malformed json", func(t *testing.T) {
		_, err := ensurePeerOutboundsBypassVPN(`{not json`)
		assert.Error(t, err)
	})
}

func TestPickInternalPort_InRange(t *testing.T) {
	for i := 0; i < 100; i++ {
		p := pickInternalPort()
		assert.GreaterOrEqual(t, int(p), internalPortMin)
		assert.Less(t, int(p), internalPortMax)
	}
}

func TestAPIError_StringFormat(t *testing.T) {
	e := &APIError{Status: 422, Body: "could not connect to peer port"}
	assert.Contains(t, e.Error(), "422")
	assert.Contains(t, e.Error(), "could not connect")
}

var _ portForwarder = (*fakeForwarder)(nil)
var _ boxService = (*fakeBoxService)(nil)
