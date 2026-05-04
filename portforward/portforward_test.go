package portforward

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIGD struct {
	mu          sync.Mutex
	addCalls    atomic.Int64
	deleteCalls atomic.Int64
	addErr      error
	deleteErr   error
	extIPErr    error
	extIP       string
	addBlock    chan struct{} // if non-nil, AddPortMapping blocks on receive
	lastAdd     mappingArgs
	lastDelete  deleteArgs
}

type mappingArgs struct {
	externalPort, internalPort uint16
	internalClient, description string
	leaseDuration               uint32
}

type deleteArgs struct {
	externalPort uint16
	protocol     string
}

func (f *fakeIGD) AddPortMapping(_ string, externalPort uint16, _ string, internalPort uint16, internalClient string, _ bool, description string, leaseDuration uint32) error {
	f.addCalls.Add(1)
	if f.addBlock != nil {
		<-f.addBlock
	}
	f.mu.Lock()
	f.lastAdd = mappingArgs{
		externalPort:   externalPort,
		internalPort:   internalPort,
		internalClient: internalClient,
		description:    description,
		leaseDuration:  leaseDuration,
	}
	f.mu.Unlock()
	return f.addErr
}

func (f *fakeIGD) DeletePortMapping(_ string, externalPort uint16, protocol string) error {
	f.deleteCalls.Add(1)
	f.mu.Lock()
	f.lastDelete = deleteArgs{externalPort: externalPort, protocol: protocol}
	f.mu.Unlock()
	return f.deleteErr
}

func (f *fakeIGD) GetExternalIPAddress() (string, error) {
	if f.extIPErr != nil {
		return "", f.extIPErr
	}
	if f.extIP == "" {
		return "203.0.113.1", nil
	}
	return f.extIP, nil
}

func newTestForwarder(t *testing.T, c *fakeIGD) *Forwarder {
	t.Helper()
	return &Forwarder{client: c, method: "fake"}
}

func TestForwarder_MapPort_HappyPath(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	m, err := f.MapPort(context.Background(), 30001, "test")
	require.NoError(t, err)
	assert.Equal(t, uint16(30001), m.ExternalPort)
	assert.Equal(t, uint16(30001), m.InternalPort)
	assert.Equal(t, "TCP", m.Protocol)
	assert.Equal(t, "fake", m.Method)
	assert.Equal(t, int64(1), c.addCalls.Load())
}

func TestForwarder_MapPort_DoubleMapRejected(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), 30001, "test")
	require.NoError(t, err)
	_, err = f.MapPort(context.Background(), 30002, "test")
	assert.ErrorContains(t, err, "already has an active mapping")
}

func TestForwarder_MapPort_PropagatesGatewayError(t *testing.T) {
	c := &fakeIGD{addErr: errors.New("conflict")}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), 30001, "test")
	assert.ErrorContains(t, err, "add port mapping")
}

// MapPort must respect the caller's context — a hung router shouldn't tie up
// Start past its deadline.
func TestForwarder_MapPort_RespectsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	c := &fakeIGD{addBlock: block}
	f := newTestForwarder(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.MapPort(ctx, 30001, "test")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	close(block) // release the leaked goroutine
}

func TestForwarder_UnmapPort_NoMappingIsNoop(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	require.NoError(t, f.UnmapPort(context.Background()))
	assert.Equal(t, int64(0), c.deleteCalls.Load())
}

func TestForwarder_UnmapPort_RemovesMapping(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), 30001, "test")
	require.NoError(t, err)

	require.NoError(t, f.UnmapPort(context.Background()))
	assert.Equal(t, int64(1), c.deleteCalls.Load())
	assert.Equal(t, uint16(30001), c.lastDelete.externalPort)
	assert.Equal(t, "TCP", c.lastDelete.protocol)

	// Calling MapPort after UnmapPort must succeed (mapping cleared).
	_, err = f.MapPort(context.Background(), 30002, "test")
	require.NoError(t, err)
}

func TestForwarder_StartRenewal_ReissuesAddPortMapping(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	// Use a short lease so the renewal interval clamps to the 1m floor; we
	// invoke the loop directly with a fast interval to avoid waiting.
	_, err := f.MapPort(context.Background(), 30001, "test")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go f.renewLoop(ctx, 20*time.Millisecond)

	deadline := time.After(2 * time.Second)
	for c.addCalls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("renewal fired only %d times", c.addCalls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
}

// Cancelling the renewal ctx must stop the loop even with a long interval.
func TestForwarder_StartRenewal_CancelsCleanly(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), 30001, "test")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.renewLoop(ctx, time.Hour)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewLoop did not exit after ctx cancel")
	}
}

func TestForwarder_ExternalIP(t *testing.T) {
	c := &fakeIGD{extIP: "203.0.113.50"}
	f := newTestForwarder(t, c)
	ip, err := f.ExternalIP(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.50", ip)
}

func TestForwarder_ExternalIP_EmptyIsError(t *testing.T) {
	f := &Forwarder{client: emptyExtIPClient{}, method: "fake"}
	_, err := f.ExternalIP(context.Background())
	assert.ErrorContains(t, err, "empty external ip")
}

type emptyExtIPClient struct{}

func (emptyExtIPClient) AddPortMapping(string, uint16, string, uint16, string, bool, string, uint32) error {
	return nil
}
func (emptyExtIPClient) DeletePortMapping(string, uint16, string) error { return nil }
func (emptyExtIPClient) GetExternalIPAddress() (string, error)         { return "", nil }

func TestForwarder_ExternalIP_PropagatesError(t *testing.T) {
	c := &fakeIGD{extIPErr: errors.New("upstream timeout")}
	f := newTestForwarder(t, c)
	_, err := f.ExternalIP(context.Background())
	assert.ErrorContains(t, err, "upstream timeout")
}

func TestLocalIP(t *testing.T) {
	// Best-effort: localIP needs working UDP. CI machines have it; offline
	// dev machines may not. Skip rather than fail if it errors.
	ip, err := LocalIP()
	if err != nil {
		t.Skipf("localIP unavailable in this environment: %v", err)
	}
	assert.NotEmpty(t, ip)
}
