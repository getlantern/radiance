package backend

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/broflake/common/helperticket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/peer"
)

// newPairingRouteTestBackend is newPeerTestBackend with a pairing key store
// that syncs the pairings-only route on change, as NewLocalBackend wires it.
func newPairingRouteTestBackend(t *testing.T, fake *fakePeerController) *LocalBackend {
	t.Helper()
	r := newPeerTestBackend(t, fake)
	fake.pairingsOnly = r.peerForPairings.Load
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
	// Closing stops the retries, so the state below is settled.
	r.closePeerClient()
	assert.False(t, r.peerForPairings.Load())
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey))
}

// A network change retries at once rather than waiting out the backoff, but
// not again until pairingRouteRetryFirst has passed since the last attempt.
func TestPairingRoute_NetworkChangeRetriesNow(t *testing.T) {
	first := pairingRouteRetryFirst
	pairingRouteRetryFirst = 200 * time.Millisecond
	t.Cleanup(func() { pairingRouteRetryFirst = first })
	fake := &fakePeerController{startErr: errors.New("no upnp")}
	r := newPairingRouteTestBackend(t, fake)

	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.Equal(t, int64(1), fake.startCalls.Load())

	r.pairingRouteNetworkChanged()
	waitPeerWork(t, r)
	assert.Equal(t, int64(1), fake.startCalls.Load(), "a burst of network events doesn't hammer the router")

	fake.startErr = nil
	r.pairingRetryMu.Lock()
	r.pairingLastAttempt = time.Now().Add(-time.Second)
	r.pairingRetryMu.Unlock()
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

// A share Start that fails and rolls the toggle back leaves sharing off, so a
// volunteer goes back to serving its pairings.
func TestPairingRoute_ShareStartFailureFallsBackToPairings(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	fake.onStart = func() {
		if settings.GetBool(settings.PeerShareEnabledKey) {
			fake.startErr = errors.New("register 4xx")
		} else {
			fake.startErr = nil
		}
	}
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.NoError(t, r.applyPeerShare(false))
	waitPeerWork(t, r)
	fake.active.Store(false)
	r.peerForPairings.Store(false)

	require.Error(t, r.applyPeerShare(true))
	waitPeerWork(t, r)
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey))
	assert.True(t, fake.IsActive(), "the pairings-only run starts again")
	assert.True(t, r.peerForPairings.Load())
}

// A sharing run that stops itself turns the toggle off, and a volunteer goes
// back to serving its pairings.
func TestPairingRoute_ShareSelfStopFallsBackToPairings(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	require.NoError(t, r.applyPeerShare(true))
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)

	fake.active.Store(false)
	r.onPeerSelfStop(errors.New("route dropped"))
	waitPeerWork(t, r)
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey))
	assert.Equal(t, int64(2), fake.startCalls.Load())
	assert.True(t, fake.IsActive())
	assert.True(t, r.peerForPairings.Load())
}

// An "off" while the run is already pairings-only, or a "help others too"
// change then, leaves the run alone: no credential rotation drops the friend.
func TestPairingRoute_RedundantChangesDontRotate(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.True(t, r.peerForPairings.Load())

	require.NoError(t, r.PatchSettings(settings.Settings{settings.PeerShareEnabledKey: false}))
	require.NoError(t, r.PatchSettings(settings.Settings{settings.PeerServePoolKey: false}))
	waitPeerWork(t, r)
	assert.Zero(t, fake.reregisterCalls.Load())
	assert.Zero(t, fake.stopCalls.Load())
	assert.Equal(t, int64(1), fake.startCalls.Load())
}

// A pairings-only run doesn't show as Share My Connection, which is off.
func TestPairingRoute_StatusShowsSharingOff(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.True(t, fake.IsActive())
	assert.False(t, r.PeerStatus().Active)
}

// With "help others too" off, a sharing run already serves only pairings, so
// handing it to or from pairings-only rotates nothing and keeps the friends'
// sessions; the sharing UI still hears of each hand-off.
func TestPairingRoute_HandOffWithoutPoolKeepsSessions(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	require.NoError(t, settings.Set(settings.PeerServePoolKey, false))
	var statuses atomic.Int64
	sub := events.Subscribe(func(peer.StatusEvent) { statuses.Add(1) })
	t.Cleanup(func() { events.Unsubscribe(sub) })
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)

	require.NoError(t, r.applyPeerShare(true))
	require.NoError(t, r.applyPeerShare(false))
	waitPeerWork(t, r)
	assert.Zero(t, fake.reregisterCalls.Load())
	assert.Zero(t, fake.stopCalls.Load())
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.True(t, r.peerForPairings.Load())
	require.Eventually(t, func() bool { return statuses.Load() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"each hand-off tells the sharing UI")
}

// A pairings-only run's relayed bytes aren't Share My Connection's.
func TestPairingRoute_RelaySnapshotShowsSharingOff(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	assert.Empty(t, r.PeerRelaySnapshot().Peers)
}

// A hand-off is judged by the registration, not the setting: with "help others
// too" just turned off but the route still registered for the pool (its
// re-register pending or failed), turning sharing off still re-registers, and
// fails closed if that fails.
func TestPairingRoute_HandOffJudgesTheRegistration(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	require.NoError(t, r.applyPeerShare(true))
	require.True(t, fake.MayServePool())
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.NoError(t, settings.Set(settings.PeerServePoolKey, false))
	fake.setReregister(func(context.Context, int64) error { return errors.New("lantern-cloud unreachable") })

	require.NoError(t, r.applyPeerShare(false))
	waitPeerWork(t, r)
	assert.Equal(t, int64(1), fake.reregisterCalls.Load(), "a pool registration is re-registered whatever the setting says")
	assert.Equal(t, int64(1), fake.stopCalls.Load(), "and the pool route is taken down when that fails")
	assert.False(t, fake.MayServePool())
}

// A pairings-only run that soon stops itself backs off like a failed start; one
// that stayed up restarts the backoff.
func TestPairingRoute_BackoffResetsOnlyAfterAStableRun(t *testing.T) {
	first, maxWait, stable := pairingRouteRetryFirst, pairingRouteRetryMax, pairingRunStableAfter
	// Retries far off, so none fires during the test.
	pairingRouteRetryFirst, pairingRouteRetryMax, pairingRunStableAfter = time.Hour, 8*time.Hour, 50*time.Millisecond
	t.Cleanup(func() { pairingRouteRetryFirst, pairingRouteRetryMax, pairingRunStableAfter = first, maxWait, stable })
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	nextWait := func() time.Duration {
		r.pairingRetryMu.Lock()
		defer r.pairingRetryMu.Unlock()
		return r.pairingRetryWait
	}
	selfStop := func() {
		fake.active.Store(false)
		r.onPeerSelfStop(errors.New("route dropped"))
	}
	r.pairingRetryMu.Lock()
	r.pairingRetryWait = 2 * time.Hour
	r.pairingRetryMu.Unlock()

	selfStop()
	assert.Equal(t, 4*time.Hour, nextWait(), "a short-lived run keeps backing off")

	r.peerForPairings.Store(true)
	fake.active.Store(true)
	r.notePairingRunStart()
	time.Sleep(2 * pairingRunStableAfter)
	selfStop()
	assert.Equal(t, 2*time.Hour, nextWait(), "a stable run restarts the backoff from its first step")
}

// Turning sharing on over a pairings-only run with "help others too" off in
// the same patch rotates nothing: the registration already matches.
func TestPairingRoute_ShareOnWithPoolOffInOnePatch(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.True(t, r.peerForPairings.Load())

	require.NoError(t, r.PatchSettings(settings.Settings{
		settings.PeerShareEnabledKey: true,
		settings.PeerServePoolKey:    false,
	}))
	waitPeerWork(t, r)
	assert.Zero(t, fake.reregisterCalls.Load())
	assert.False(t, r.peerForPairings.Load())
	assert.True(t, settings.GetBool(settings.PeerShareEnabledKey))
}

// Entering sharing from pairings-only re-registers exactly when "help others
// too" is on, whatever a rotation in flight leaves the registration looking
// like: it's the only case in which serve_pool changes.
func TestPairingRoute_EnteringSharingFollowsTheSetting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		servesPool bool
		want       int64
	}{
		{"help others too on", true, 1},
		{"help others too off", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePeerController{}
			r := newPairingRouteTestBackend(t, fake)
			require.NoError(t, settings.Set(settings.PeerServePoolKey, tc.servesPool))
			r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
			waitPeerWork(t, r)
			// An hourly rotation in flight: the registration may be anything.
			fake.poolMaybe.Store(true)

			require.NoError(t, r.applyPeerShare(true))
			require.Eventually(t, func() bool { return fake.reregisterCalls.Load() == tc.want }, 2*time.Second, 5*time.Millisecond)
			waitPeerWork(t, r)
			assert.Equal(t, tc.want, fake.reregisterCalls.Load())
		})
	}
}

// A failed pairings-only start leaves its error on the client; that isn't a
// Share My Connection failure, so it stays hidden through the retry backoff
// until sharing takes over.
func TestPairingRoute_FailedStartDoesntShowAsSharingError(t *testing.T) {
	fake := &fakePeerController{startErr: errors.New("no upnp")}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.False(t, r.peerForPairings.Load())
	require.Equal(t, peer.PhaseError, fake.CurrentStatus().Phase)
	assert.Equal(t, peer.Status{}, r.PeerStatus())

	fake.startErr = nil
	require.NoError(t, r.applyPeerShare(true))
	assert.True(t, r.PeerStatus().Active, "a sharing run's status shows again")
}

// A burst of network events reserves one attempt: the cooldown holds even when
// they arrive while a start is still waiting for the toggle lock.
func TestPairingRoute_NetworkBurstQueuesOneAttempt(t *testing.T) {
	first := pairingRouteRetryFirst
	pairingRouteRetryFirst = time.Hour
	t.Cleanup(func() { pairingRouteRetryFirst = first })
	fake := &fakePeerController{startErr: errors.New("no upnp")}
	r := newPairingRouteTestBackend(t, fake)
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.Equal(t, int64(1), fake.startCalls.Load())
	r.pairingRetryMu.Lock()
	r.pairingLastAttempt = time.Now().Add(-2 * time.Hour)
	r.pairingRetryMu.Unlock()

	r.peerToggleMu.Lock()
	r.pairingRouteNetworkChanged()
	r.pairingRouteNetworkChanged()
	r.peerToggleMu.Unlock()
	waitPeerWork(t, r)
	assert.Equal(t, int64(2), fake.startCalls.Load(), "one attempt for the burst")
}

// When a sharing start fails and a pairings-only run takes the client over,
// the sharing status keeps saying why sharing failed rather than going blank.
func TestPairingRoute_SharingFailureStaysVisible(t *testing.T) {
	fake := &fakePeerController{}
	r := newPairingRouteTestBackend(t, fake)
	fake.onStart = func() {
		if settings.GetBool(settings.PeerShareEnabledKey) {
			fake.startErr = errors.New("register 4xx")
		} else {
			fake.startErr = nil
		}
	}
	r.helperPairingKeys.Set(pairingKeys(helperticket.RoleVolunteer))
	waitPeerWork(t, r)
	require.NoError(t, r.applyPeerShare(false))
	waitPeerWork(t, r)
	fake.active.Store(false)
	r.peerForPairings.Store(false)

	require.Error(t, r.applyPeerShare(true))
	waitPeerWork(t, r)
	require.True(t, fake.CurrentStatus().PairingsOnly, "a pairings-only run took the client over")
	st := r.PeerStatus()
	assert.Equal(t, peer.PhaseError, st.Phase)
	assert.Equal(t, "register 4xx", st.Error)
}
