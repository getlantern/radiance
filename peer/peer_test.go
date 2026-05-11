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

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/events"
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
	verifyStatus       int
	heartbeatStatus    int
	deregisterStatus   int
	registerCount      atomic.Int64
	verifyCount        atomic.Int64
	heartbeatCount     atomic.Int64
	deregisterCount    atomic.Int64
	registerDeviceID   atomic.Value // string
	verifyDeviceID     atomic.Value // string
	heartbeatDeviceID  atomic.Value // string
	deregisterDeviceID atomic.Value // string
	lastRegisterReq    atomic.Value // RegisterRequest
}

func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{
		t:                t,
		registerStatus:   http.StatusOK,
		verifyStatus:     http.StatusOK,
		heartbeatStatus:  http.StatusOK,
		deregisterStatus: http.StatusOK,
		registerResp: RegisterResponse{
			RouteID:                  "00000000-0000-0000-0000-000000000123",
			ServerConfig:             `{"inbounds": [{"type":"samizdat","tag":"samizdat-in"}]}`,
			HeartbeatIntervalSeconds: 60,
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/peer/register", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("/peer/verify", func(w http.ResponseWriter, r *http.Request) {
		s.verifyCount.Add(1)
		s.verifyDeviceID.Store(r.Header.Get("X-Lantern-Device-Id"))
		if s.verifyStatus != http.StatusOK {
			http.Error(w, "verify failed", s.verifyStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/peer/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		s.heartbeatCount.Add(1)
		s.heartbeatDeviceID.Store(r.Header.Get("X-Lantern-Device-Id"))
		if s.heartbeatStatus != http.StatusOK {
			http.Error(w, "heartbeat failed", s.heartbeatStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/peer/deregister", func(w http.ResponseWriter, r *http.Request) {
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

// TestClient_StatusEventEmittedOnStartAndStop pins the full lifecycle
// phase sequence: Start fires one StatusEvent per stage so the UI can
// render granular progress (mapping port → registering → verifying →
// serving) instead of a single active/inactive flip. Stop fires
// stopping → idle on the way back down.
//
// Subscribers (the IPC SSE handler in production) need every edge so the
// UI can render fresh state without polling.
func TestClient_StatusEventEmittedOnStartAndStop(t *testing.T) {
	fwd := &fakeForwarder{}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	// Buffer must exceed total emit count (6 on Start: mapping → detecting
	// → registering → starting_proxy → verifying → serving; 2 on Stop:
	// stopping → idle) or the subscriber's send blocks and emits drop.
	got := make(chan StatusEvent, 16)
	sub := events.Subscribe(func(evt StatusEvent) {
		got <- evt
	})
	defer sub.Unsubscribe()

	require.NoError(t, c.Start(context.Background()))

	wantStartPhases := []Phase{
		PhaseMappingPort,
		PhaseDetectingIP,
		PhaseRegistering,
		PhaseStartingBox,
		PhaseVerifying,
		PhaseServing,
	}
	for _, want := range wantStartPhases {
		select {
		case evt := <-got:
			assert.Equal(t, want, evt.Status.Phase, "wrong phase in Start sequence")
			if want == PhaseServing {
				assert.True(t, evt.Status.Active, "active must be true on serving")
				assert.NotEmpty(t, evt.Status.RouteID, "route_id must be set on serving")
			} else {
				assert.False(t, evt.Status.Active, "active must be false on intermediate phase %q", want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no Start status event for phase %q within 1s", want)
		}
	}

	require.NoError(t, c.Stop(context.Background()))
	for _, want := range []Phase{PhaseStopping, PhaseIdle} {
		select {
		case evt := <-got:
			assert.Equal(t, want, evt.Status.Phase, "wrong phase in Stop sequence")
			assert.False(t, evt.Status.Active, "active must be false during stop")
		case <-time.After(time.Second):
			t.Fatalf("no Stop status event for phase %q within 1s", want)
		}
	}
}

// TestClient_StatusEventOnStartError surfaces a Start failure to the UI
// via PhaseError with the wrapped error message. Without this, a user
// who clicks SmC-on and hits e.g. a UPnP failure sees the toggle silently
// flip back without any diagnostic.
func TestClient_StatusEventOnStartError(t *testing.T) {
	fwd := &fakeForwarder{mapErr: errors.New("upnp gateway refused mapping")}
	box := &fakeBoxService{}
	srv := newStubServer(t)
	c := newTestClient(t, fwd, box, srv)

	got := make(chan StatusEvent, 16)
	sub := events.Subscribe(func(evt StatusEvent) { got <- evt })
	defer sub.Unsubscribe()

	err := c.Start(context.Background())
	require.Error(t, err)

	var sawError bool
	deadline := time.After(time.Second)
	for !sawError {
		select {
		case evt := <-got:
			if evt.Status.Phase == PhaseError {
				sawError = true
				assert.False(t, evt.Status.Active)
				assert.Contains(t, evt.Status.Error, "upnp gateway refused mapping",
					"error message must surface so the UI can render a real diagnostic")
			}
		case <-deadline:
			t.Fatal("no PhaseError status event within 1s")
		}
	}
}

var _ portForwarder = (*fakeForwarder)(nil)
var _ boxService = (*fakeBoxService)(nil)

// TestDefaultBuildBoxService_DecodesSamizdatInbound is the regression net
// for the "missing inbound fields registry in context" failure that bit
// us live: defaultBuildBoxService used to call libbox.NewServiceWithContext
// with a fresh ctx that didn't have the lantern-box protocol registries
// (samizdat, reflex, …) plumbed in, so the JSON decoder couldn't resolve
// inbounds[0].type="samizdat" → libbox.NewServiceWithContext returned an
// error → applyPeerShare rolled the toggle back. The integration tests
// stub BuildBoxService entirely, so neither the libbox setup nor the
// samizdat decoder were exercised in CI.
//
// Calling defaultBuildBoxService directly with a minimal samizdat-inbound
// options JSON walks the actual decode path. If the registry is missing
// in the ctx that defaultBuildBoxService produces, libbox returns the
// "missing inbound fields registry" error and this test fails before any
// of the runtime cycle (rebuild, redeploy, toggle UI, dial-back) — what
// used to take a 5-minute round-trip is now a 0.1s test failure.
func TestDefaultBuildBoxService_DecodesSamizdatInbound(t *testing.T) {
	// Minimal but complete samizdat inbound — every field that
	// option.SamizdatInboundOptions's json tags require to round-trip.
	// Values are placeholders; we don't run the box, just decode.
	const opts = `{
		"inbounds": [{
			"type": "samizdat",
			"tag": "samizdat-in",
			"listen": "127.0.0.1",
			"listen_port": 5698,
			"private_key": "0000000000000000000000000000000000000000000000000000000000000000",
			"short_ids": ["0000000000000000"],
			"cert_pem": "-----BEGIN CERTIFICATE-----\nMIIBhTCCASugAwIBAgIQCHOFXAcuEzPfyHK6LdwxwzAKBggqhkjOPQQDAjATMREw\nDwYDVQQKEwhJbnRlcm5ldDAeFw0yNjA1MDYwMDAwMDBaFw0yNzA1MDYwMDAwMDBa\nMBMxETAPBgNVBAoTCEludGVybmV0MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\nb6xQ7UDl11wL/8mZwLxrNqx6JJ+FczIw9V0a9Q3CYUYFGu5DzVyDUwmfVTZiQ+wR\nkQXjrkAwsOWK99JsM3R2bqNIMEYwDgYDVR0PAQH/BAQDAgeAMBMGA1UdJQQMMAoG\nCCsGAQUFBwMBMAwGA1UdEwEB/wQCMAAwEQYDVR0RBAowCIIGdGVzdC5xMAoGCCqG\nSM49BAMCA0kAMEYCIQCqhyaQaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaIh\nAOaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa=\n-----END CERTIFICATE-----\n",
			"key_pem": "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaoAoGCCqGSM49\nAwEHoUQDQgAEb6xQ7UDl11wL/8mZwLxrNqx6JJ+FczIw9V0a9Q3CYUYFGu5DzVyD\nUwmfVTZiQ+wRkQXjrkAwsOWK99JsM3R2bg==\n-----END EC PRIVATE KEY-----\n",
			"masquerade_domain": "example.com"
		}]
	}`

	bs, err := defaultBuildBoxService(context.Background(), opts)
	require.NoError(t, err, "defaultBuildBoxService must decode a samizdat inbound — "+
		"the lantern-box protocol registries have to be in ctx")
	require.NotNil(t, bs)
	// We never call Start; just verifying the decode path. Close drops
	// any background structures libbox might have stood up.
	_ = bs.Close()
}

// All four peer endpoints must carry the same standard header set as
// /config-new (X-Lantern-Config-Client-IP in particular). The server's
// util.ClientIPWithAddr prefers that header over X-Forwarded-For and
// RemoteAddr; without it, register/verify resolve a different IP than
// radiance has detected, and the server's verifier dials an address the
// peer's listener isn't bound to.
func TestAPI_ForwardsCommonHeaders(t *testing.T) {
	const fakePublicIP = "198.51.100.7"
	common.SetPublicIP(fakePublicIP)
	t.Cleanup(func() { common.SetPublicIP("") })

	type capture struct {
		clientIP  string
		deviceID  string
		platform  string
		appName   string
		userAgent string
	}
	captured := make(map[string]capture)
	var mu sync.Mutex
	record := func(path string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		captured[path] = capture{
			clientIP:  r.Header.Get(common.ClientIPHeader),
			deviceID:  r.Header.Get(common.DeviceIDHeader),
			platform:  r.Header.Get(common.PlatformHeader),
			appName:   r.Header.Get(common.AppNameHeader),
			userAgent: r.Header.Get("User-Agent"),
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/peer/register", func(w http.ResponseWriter, r *http.Request) {
		record("/peer/register", r)
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			RouteID:                  "00000000-0000-0000-0000-000000000123",
			ServerConfig:             `{}`,
			HeartbeatIntervalSeconds: 60,
		})
	})
	mux.HandleFunc("/peer/verify", func(w http.ResponseWriter, r *http.Request) {
		record("/peer/verify", r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/peer/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		record("/peer/heartbeat", r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/peer/deregister", func(w http.ResponseWriter, r *http.Request) {
		record("/peer/deregister", r)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	api := NewAPI(srv.Client(), srv.URL, "test-device-id")
	ctx := context.Background()

	_, err := api.Register(ctx, RegisterRequest{ExternalIP: "203.0.113.42", ExternalPort: 5698, InternalPort: 35698})
	require.NoError(t, err)
	require.NoError(t, api.Verify(ctx, "00000000-0000-0000-0000-000000000123"))
	require.NoError(t, api.Heartbeat(ctx, "00000000-0000-0000-0000-000000000123"))
	require.NoError(t, api.Deregister(ctx, "00000000-0000-0000-0000-000000000123"))

	for _, path := range []string{"/peer/register", "/peer/verify", "/peer/heartbeat", "/peer/deregister"} {
		mu.Lock()
		c, ok := captured[path]
		mu.Unlock()
		require.True(t, ok, "no request captured for %s", path)
		assert.Equal(t, fakePublicIP, c.clientIP,
			"%s must forward radiance's detected public IP via %s "+
				"so server-side ClientIPWithAddr resolves the same IP it does for /config-new",
			path, common.ClientIPHeader)
		assert.Equal(t, "test-device-id", c.deviceID, "%s must carry %s", path, common.DeviceIDHeader)
		assert.NotEmpty(t, c.platform, "%s must carry %s", path, common.PlatformHeader)
		assert.NotEmpty(t, c.appName, "%s must carry %s", path, common.AppNameHeader)
	}
}
