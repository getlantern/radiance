package backend

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/peer"
)

func TestBackend(t *testing.T) {}

type fakePeerController struct {
	startCalls atomic.Int64
	stopCalls  atomic.Int64
	startErr   error
	active     atomic.Bool
}

func (f *fakePeerController) Start(_ context.Context) error {
	f.startCalls.Add(1)
	if f.startErr != nil {
		return f.startErr
	}
	f.active.Store(true)
	return nil
}

func (f *fakePeerController) Stop(_ context.Context) error {
	f.stopCalls.Add(1)
	f.active.Store(false)
	return nil
}

func (f *fakePeerController) IsActive() bool          { return f.active.Load() }
func (f *fakePeerController) CurrentStatus() peer.Status { return peer.Status{Active: f.active.Load()} }

// newPeerTestBackend wires a minimal LocalBackend with only the fields
// applyPeerShare touches. settings is initialized to a fresh tempdir per test
// so the rollback path doesn't leak across runs.
func newPeerTestBackend(t *testing.T, fake *fakePeerController) *LocalBackend {
	t.Helper()
	require.NoError(t, settings.InitSettings(t.TempDir()))
	t.Cleanup(settings.Reset)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &LocalBackend{ctx: ctx, peerClient: fake}
}

func TestApplyPeerShare_StartsOnEnable(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)

	require.NoError(t, r.applyPeerShare(true))
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.Equal(t, int64(0), fake.stopCalls.Load())
	assert.True(t, fake.IsActive())
}

func TestApplyPeerShare_StopsOnDisable(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	fake.active.Store(true)

	require.NoError(t, r.applyPeerShare(false))
	assert.Equal(t, int64(0), fake.startCalls.Load())
	assert.Equal(t, int64(1), fake.stopCalls.Load())
	assert.False(t, fake.IsActive())
}

// On a Start failure we surface the error so the Dart side can roll back
// its UI, AND we flip the persisted setting back to false so the user-visible
// state matches reality on the next read.
func TestApplyPeerShare_StartFailureRollsBackSetting(t *testing.T) {
	fake := &fakePeerController{startErr: errors.New("no upnp")}
	r := newPeerTestBackend(t, fake)

	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))
	require.True(t, settings.GetBool(settings.PeerShareEnabledKey))

	err := r.applyPeerShare(true)
	require.Error(t, err)
	assert.ErrorContains(t, err, "no upnp")
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey),
		"setting must roll back to false after a Start failure")
	assert.False(t, fake.IsActive())
}

func TestPeerStatus_Accessor(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	fake.active.Store(true)

	got := r.PeerStatus()
	assert.True(t, got.Active)
}

func TestResumePeerShare_NoopWhenSettingOff(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)

	r.resumePeerShareIfEnabled()
	r.peerWG.Wait()
	assert.Equal(t, int64(0), fake.startCalls.Load())
}

func TestResumePeerShare_StartsWhenSettingOn(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))

	r.resumePeerShareIfEnabled()
	r.peerWG.Wait()
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.True(t, fake.IsActive())
}

// Close must wait for an in-flight auto-resume Start before tearing down,
// then call Stop on the active session — otherwise we leave a registered
// route + open box behind on shutdown.
func TestClose_WaitsForResumeAndStopsActivePeer(t *testing.T) {
	startGate := make(chan struct{})
	fake := &slowStartFake{gate: startGate}
	r := newCloseableTestBackend(t, fake)
	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))

	r.resumePeerShareIfEnabled()

	closeReturned := make(chan struct{})
	go func() {
		r.Close()
		close(closeReturned)
	}()

	// Close must NOT return while the resume goroutine is still in Start.
	select {
	case <-closeReturned:
		t.Fatal("Close returned before in-flight resume Start completed")
	case <-time.After(50 * time.Millisecond):
	}

	// Release Start. It returns nil → peer becomes active. Close must then
	// observe IsActive() and call Stop.
	close(startGate)
	select {
	case <-closeReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after resume Start unblocked")
	}
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.Equal(t, int64(1), fake.stopCalls.Load())
}

// slowStartFake blocks on gate until the test releases it, simulating a
// long UPnP discovery so we can race Close against Start.
type slowStartFake struct {
	startCalls atomic.Int64
	stopCalls  atomic.Int64
	active     atomic.Bool
	gate       chan struct{}
}

func (f *slowStartFake) Start(ctx context.Context) error {
	f.startCalls.Add(1)
	select {
	case <-f.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	f.active.Store(true)
	return nil
}
func (f *slowStartFake) Stop(_ context.Context) error {
	f.stopCalls.Add(1)
	f.active.Store(false)
	return nil
}
func (f *slowStartFake) IsActive() bool             { return f.active.Load() }
func (f *slowStartFake) CurrentStatus() peer.Status { return peer.Status{Active: f.active.Load()} }

// newCloseableTestBackend mirrors newPeerTestBackend but provides the fields
// Close needs (closeOnce, stopChan, cancel) so we can exercise the shutdown
// path end-to-end.
func newCloseableTestBackend(t *testing.T, fake peerController) *LocalBackend {
	t.Helper()
	require.NoError(t, settings.InitSettings(t.TempDir()))
	t.Cleanup(settings.Reset)
	ctx, cancel := context.WithCancel(context.Background())
	return &LocalBackend{
		ctx:        ctx,
		cancel:     cancel,
		peerClient: fake,
		stopChan:   make(chan struct{}),
		closeOnce:  sync.Once{},
	}
}

// Verify the PatchSettings dispatch actually routes PeerShareEnabledKey to
// applyPeerShare. A typo on the diff key would silently break the toggle.
func TestPatchSettings_PeerShareDispatches(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)

	require.NoError(t, r.PatchSettings(settings.Settings{settings.PeerShareEnabledKey: true}))
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.True(t, fake.IsActive())

	require.NoError(t, r.PatchSettings(settings.Settings{settings.PeerShareEnabledKey: false}))
	assert.Equal(t, int64(1), fake.stopCalls.Load())
	assert.False(t, fake.IsActive())
}
