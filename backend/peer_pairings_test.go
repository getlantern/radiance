package backend

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getlantern/broflake/common/helperticket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/helper"
)

// newPairingRouteTestBackend is newPeerTestBackend with a pairing key store
// that syncs the pairings-only route on change, as NewLocalBackend wires it.
func newPairingRouteTestBackend(t *testing.T, fake *fakePeerController) *LocalBackend {
	t.Helper()
	r := newPeerTestBackend(t, fake)
	r.helperPairingKeys = &helper.PairingKeys{}
	r.helperPairingKeys.SetOnChange(r.pairingKeysChanged)
	t.Cleanup(r.closePeerClient)
	return r
}

func pairingKeys(roles ...helperticket.Role) map[string]helper.PairingKey {
	keys := make(map[string]helper.PairingKey, len(roles))
	for i, role := range roles {
		keys[string(rune('a'+i))] = helper.PairingKey{Key: bytes.Repeat([]byte{byte(i + 1)}, 32), Role: string(role)}
	}
	return keys
}

// A volunteer with Share My Connection off starts its route for its pairings,
// pairings-only, and stops it when the last pairing ends.
func TestPairingRoute_FollowsVolunteerPairings(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)

	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.True(t, fake.IsActive())
	assert.True(t, r.peerForPairings.Load(), "the run serves pairings only")
	assert.True(t, r.peerShareActive(), "the reverse client must leave the manual port alone")
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey), "the share toggle stays off")

	r.helperPairingKeys.Set(nil)
	waitPeerWork(t, r)
	assert.Equal(t, int64(1), fake.stopCalls.Load())
	assert.False(t, fake.IsActive())
	assert.False(t, r.peerForPairings.Load())
}

// Only being someone's volunteer starts the route; an inviter pairing doesn't.
func TestPairingRoute_InviterPairingDoesNotStart(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)

	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleInviter))
	waitPeerWork(t, r)
	assert.Zero(t, fake.startCalls.Load())
}

// A router that won't map a port fails the start; it is tried again later,
// and the share toggle is never touched.
func TestPairingRoute_RetriesAFailedStart(t *testing.T) {
	first := pairingRouteRetryFirst
	pairingRouteRetryFirst = 20 * time.Millisecond
	t.Cleanup(func() { pairingRouteRetryFirst = first })

	fake := &fakePeerController{startErr: errors.New("no upnp")}
	r := newPairingRouteTestBackend(t, fake)

	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	require.Eventually(t, func() bool { return fake.startCalls.Load() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"a failed start is retried")
	assert.False(t, r.peerForPairings.Load())
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey))
}

// A network change retries at once rather than waiting out the backoff.
func TestPairingRoute_NetworkChangeRetriesNow(t *testing.T) {
	fake := &fakePeerController{startErr: errors.New("no upnp")}
	r := newPairingRouteTestBackend(t, fake)

	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.Equal(t, int64(1), fake.startCalls.Load())

	fake.startErr = nil
	r.pairingRouteNetworkChanged()
	waitPeerWork(t, r)
	assert.Equal(t, int64(2), fake.startCalls.Load())
	assert.True(t, r.peerForPairings.Load())
}

// Share My Connection and the pairings-only route are one client on one port
// mapping: turning sharing on takes over a pairings-only run, and turning it
// off hands back, each by re-registering rather than restarting.
func TestPairingRoute_HandsOffWithShareToggle(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.True(t, r.peerForPairings.Load())

	require.NoError(t, r.applyPeerShare(true))
	require.Eventually(t, func() bool { return fake.reregisterCalls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(1), fake.startCalls.Load(), "the running client is kept")
	assert.False(t, r.peerForPairings.Load())

	require.NoError(t, r.applyPeerShare(false))
	assert.Equal(t, int64(2), fake.reregisterCalls.Load(), "turning sharing off re-registers before it returns")
	waitPeerWork(t, r)
	assert.Zero(t, fake.stopCalls.Load(), "the mapping is kept")
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.True(t, fake.IsActive())
	assert.True(t, r.peerForPairings.Load())
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey))
}

// Turning sharing off fails closed: if the re-register that drops the pool
// fails, the client stops rather than leaving a pool route up, and the
// pairings-only run starts afresh, registering serve_pool=false from the start.
func TestPairingRoute_ShareOffFailsClosed(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	require.NoError(t, r.applyPeerShare(true))
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	fake.setReregister(func(context.Context, int64) error { return errors.New("lantern-cloud unreachable") })

	require.NoError(t, r.applyPeerShare(false))
	waitPeerWork(t, r)
	assert.Equal(t, int64(1), fake.stopCalls.Load(), "the pool route is taken down")
	assert.Equal(t, int64(2), fake.startCalls.Load(), "a fresh pairings-only run starts")
	assert.True(t, fake.IsActive())
	assert.True(t, r.peerForPairings.Load())
}

// With no volunteer pairing, turning sharing off stops the client as before.
func TestPairingRoute_ShareOffWithoutPairingsStops(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	require.NoError(t, r.applyPeerShare(true))

	require.NoError(t, r.applyPeerShare(false))
	waitPeerWork(t, r)
	assert.Equal(t, int64(1), fake.stopCalls.Load())
	assert.False(t, fake.IsActive())
	assert.False(t, r.peerForPairings.Load())
}

// With sharing on, its toggle governs the client: pairing changes don't stop it.
func TestPairingRoute_LeavesSharingAlone(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	require.NoError(t, r.applyPeerShare(true))

	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	r.helperPairingKeys.Set(nil)
	waitPeerWork(t, r)
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.Zero(t, fake.stopCalls.Load())
	assert.False(t, r.peerForPairings.Load())
}

// A pairings-only run that stops itself is retried and leaves the share toggle
// as it was.
func TestPairingRoute_SelfStopRetries(t *testing.T) {
	first := pairingRouteRetryFirst
	pairingRouteRetryFirst = 20 * time.Millisecond
	t.Cleanup(func() { pairingRouteRetryFirst = first })

	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.True(t, r.peerForPairings.Load())

	fake.active.Store(false)
	r.onPeerSelfStop(errors.New("route dropped"))
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey))
	require.Eventually(t, func() bool { return fake.startCalls.Load() == 2 && fake.IsActive() }, 2*time.Second, 5*time.Millisecond)
	assert.True(t, r.peerForPairings.Load())
}
