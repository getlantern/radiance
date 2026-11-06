package backend

import (
	"bytes"
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/getlantern/common"
	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/broflake/common/helperticket"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/peer"
	"github.com/getlantern/radiance/vpn"
)

// fakeReverse stands in for the reverse client. With a box, it adds tags as
// reverse outbounds on Start and tears the box down on Stop, as the client's
// first cycle and teardown do.
type fakeReverse struct {
	starts, stops, networks, pairings atomic.Int32
	running                           atomic.Bool
	box                               peer.ReverseBox
	tags                              []string
}

func (f *fakeReverse) Start(context.Context) error {
	f.starts.Add(1)
	if f.box != nil {
		if err := f.box.SetReverseOutbounds(f.tags); err != nil {
			return err
		}
	}
	f.running.Store(true)
	return nil
}

func (f *fakeReverse) Stop(context.Context) {
	f.stops.Add(1)
	if f.box != nil {
		_ = f.box.SetReversePortal(nil, "")
	}
	f.running.Store(false)
}

func (f *fakeReverse) NetworkChanged()  { f.networks.Add(1) }
func (f *fakeReverse) PairingsChanged() { f.pairings.Add(1) }

const reverseSettle = 150 * time.Millisecond

func newReverseTestBackend(t *testing.T, unsupported bool) (*LocalBackend, *fakeReverse) {
	t.Helper()
	r, fake := newUnstartedReverseTestBackend(t, t.TempDir(), unsupported)
	r.startReverse()
	return r, fake
}

func newUnstartedReverseTestBackend(t *testing.T, dataDir string, unsupported bool) (*LocalBackend, *fakeReverse) {
	t.Helper()
	prev := reverseUnsupported
	reverseUnsupported = func() bool { return unsupported }
	t.Cleanup(func() { reverseUnsupported = prev })
	r := newPersonalProxiesBackendIn(t, dataDir)
	r.helperPairingKeys = &helper.PairingKeys{}
	fake := &fakeReverse{}
	r.useReverseClient(fake)
	t.Cleanup(r.stopReverse)
	return r, fake
}

func notRunning(f *fakeReverse) func() bool { return func() bool { return !f.running.Load() } }

func emitVPNStatus(s vpn.VPNStatus) { events.Emit(vpn.StatusUpdateEvent{Status: s}) }

func emitReverseFeature(on bool) {
	events.Emit(config.NewConfigEvent{New: &config.Config{Features: map[string]bool{C.UNBOUNDED: true, helperReverseFeature: on}}})
}

func TestReverseRunsWhileTheVPNIsConnectedAndTheFeatureIsOn(t *testing.T) {
	_, fake := newReverseTestBackend(t, false)

	emitReverseFeature(true)
	assert.Never(t, fake.running.Load, reverseSettle, 10*time.Millisecond, "the VPN is down")

	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)

	emitVPNStatus(vpn.Restarting)
	assert.Never(t, func() bool { return !fake.running.Load() }, reverseSettle, 10*time.Millisecond,
		"a restart keeps the client, whose state the new tunnel restores")
	emitVPNStatus(vpn.Connected)
	assert.Never(t, func() bool { return fake.starts.Load() != 1 }, reverseSettle, 10*time.Millisecond)

	emitVPNStatus(vpn.Disconnected)
	require.Eventually(t, notRunning(fake), time.Second, 5*time.Millisecond)

	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)

	emitReverseFeature(false)
	require.Eventually(t, notRunning(fake), time.Second, 5*time.Millisecond)
	assert.Equal(t, int32(2), fake.starts.Load())
	assert.Equal(t, int32(2), fake.stops.Load())
}

func TestReverseStopsOnAnErrorStatus(t *testing.T) {
	_, fake := newReverseTestBackend(t, false)
	emitReverseFeature(true)
	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
	emitVPNStatus(vpn.ErrorStatus)
	require.Eventually(t, notRunning(fake), time.Second, 5*time.Millisecond)
}

func TestReverseNeverStartsWhereUnsupported(t *testing.T) {
	_, fake := newReverseTestBackend(t, true)
	emitReverseFeature(true)
	emitVPNStatus(vpn.Connected)
	assert.Never(t, func() bool { return fake.starts.Load() > 0 }, reverseSettle, 10*time.Millisecond)
}

func TestReverseStopsWithTheBackend(t *testing.T) {
	r, fake := newReverseTestBackend(t, false)
	emitReverseFeature(true)
	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
	r.stopReverse()
	assert.False(t, fake.running.Load())
}

func TestReverseHearsPairingKeyChanges(t *testing.T) {
	r, fake := newReverseTestBackend(t, false)
	key := bytes.Repeat([]byte{7}, 32)
	r.SetHelperPairingKeys(map[string]helper.PairingKey{"p1": {Key: key, Role: string(helperticket.RoleInviter)}})
	assert.Equal(t, int32(1), fake.pairings.Load())
	r.SetHelperPairingKeys(map[string]helper.PairingKey{"p1": {Key: key, Role: string(helperticket.RoleInviter)}})
	assert.Equal(t, int32(1), fake.pairings.Load(), "an unchanged set is no change")
}

func TestReverseHearsNetworkResets(t *testing.T) {
	r, fake := newReverseTestBackend(t, false)
	r.ResetNetwork()
	r.ResetNetworkForMonitor(nil)
	assert.Equal(t, int32(2), fake.networks.Load())
}

func TestReverseStartsWhenTheVPNIsAlreadyConnected(t *testing.T) {
	dataDir := t.TempDir()
	writeCachedConfig(t, dataDir, &config.Config{Features: map[string]bool{helperReverseFeature: true}})
	r, fake := newUnstartedReverseTestBackend(t, dataDir, false)
	r.startReverseWith(func() vpn.VPNStatus { return vpn.Connected })
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
}

func TestReverseDoesNotStartWhileConnecting(t *testing.T) {
	_, fake := newReverseTestBackend(t, false)
	emitReverseFeature(true)
	emitVPNStatus(vpn.Connecting)
	assert.Never(t, fake.running.Load, reverseSettle, 10*time.Millisecond)
}

const reversePairing = "pairing-b"

func inviterKey(t *testing.T, r *LocalBackend, pairingIDs ...string) {
	t.Helper()
	keys := map[string]helper.PairingKey{}
	for _, id := range pairingIDs {
		keys[id] = helper.PairingKey{Key: bytes.Repeat([]byte{7}, 32), Role: string(helperticket.RoleInviter)}
	}
	r.SetHelperPairingKeys(keys)
}

// newReverseFriendBackend is a backend whose fake reverse client, once
// started, publishes ppFriendB's reverse outbound for reversePairing, with
// ppFriendB selected.
func newReverseFriendBackend(t *testing.T, dataDir string) (*LocalBackend, *fakeReverse) {
	t.Helper()
	r, fake := newUnstartedReverseTestBackend(t, dataDir, false)
	r.reverseFriends = loadReverseFriends(filepath.Join(dataDir, reverseFriendsFileName))
	inviterKey(t, r, reversePairing)
	r.reverseFriends.setTags(map[string]string{reversePairing: ppFriendB})
	fake.box, fake.tags = reverseBox{r: r}, []string{lbA.ReverseHelperTag(ppFriendB)}
	r.startSelectionFallbackListener()
	r.startReverse()
	emitReverseFeature(true)
	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
	r.persistSelection(vpn.PersonalProxySelection(ppFriendB))
	return r, fake
}

// While the VPN is down a reverse-only friend has no outbound, and the paths
// that clear a missing selection run: the Disconnected listener and a config
// update. The friend's pairing has not ended, so the selection stands.
func TestReverseKeepsASelectionWhileTheVPNIsDown(t *testing.T) {
	r, fake := newReverseFriendBackend(t, t.TempDir())
	friend := vpn.PersonalProxySelection(ppFriendB)

	emitVPNStatus(vpn.Disconnected)
	require.Eventually(t, notRunning(fake), time.Second, 5*time.Millisecond)
	assert.Empty(t, r.reverseOutbounds())
	r.applyConfig(cachedConfig(), true)
	r.clearSelectedIfMissing()
	assert.Equal(t, friend, selectedTag(t))
	assert.NoError(t, r.checkConnectSelection(friend))

	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
	assert.Equal(t, friend, selectedTag(t))
}

func TestReverseKeepsASelectionAcrossALaunch(t *testing.T) {
	dataDir := t.TempDir()
	r, _ := newReverseFriendBackend(t, dataDir)
	r.stopReverse()
	require.Empty(t, r.reverseOutbounds())

	next := newPersonalProxiesBackendIn(t, dataDir)
	next.reverseFriends = loadReverseFriends(filepath.Join(dataDir, reverseFriendsFileName))
	next.clearSelectedIfMissing()
	assert.Equal(t, vpn.PersonalProxySelection(ppFriendB), selectedTag(t))
}

func TestReverseClearsASelectionWhenItsPairingEnds(t *testing.T) {
	r, fake := newReverseFriendBackend(t, t.TempDir())
	emitVPNStatus(vpn.Disconnected)
	require.Eventually(t, notRunning(fake), time.Second, 5*time.Millisecond)

	inviterKey(t, r, "pairing-other")
	require.Eventually(t, func() bool { return selectedTag(t) == vpn.AutoSelectTag }, time.Second, 5*time.Millisecond)
	assert.False(t, r.reverseFriends.has(ppFriendB))
}

// lantern-cloud's kill switch, and an address loss, take the portal and
// outbounds down while the pairing goes on.
func TestReverseKeepsASelectionThroughAWithdrawal(t *testing.T) {
	r, _ := newReverseFriendBackend(t, t.TempDir())
	box := reverseBox{r: r}
	require.NoError(t, box.SetReverseOutbounds(nil))
	require.NoError(t, box.SetReversePortal(nil, ""))
	r.applyConfig(cachedConfig(), true)
	assert.Equal(t, vpn.PersonalProxySelection(ppFriendB), selectedTag(t))
}

// A tunnel that came up while the friend had no outbound runs on a fallback
// until the outbound returns.
func TestReverseBoxRestoresTheSavedFriendInTheTunnel(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	tunnel := &fakeHelperTunnel{live: map[string]string{}, selected: vpn.AutoSelectTag}
	tunnel.attach(r)
	r.reverseFriends = &reverseFriends{byPairing: map[string]string{pairingHash(reversePairing): ppFriendB}}
	friend := vpn.PersonalProxySelection(ppFriendB)
	r.persistSelection(friend)
	require.Equal(t, friend, selectedTag(t))

	require.NoError(t, reverseBox{r: r}.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendA)}))
	assert.Equal(t, vpn.AutoSelectTag, tunnel.current(), "another friend's outbound")
	require.NoError(t, reverseBox{r: r}.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendB)}))
	assert.Equal(t, friend, tunnel.current())
}

// A connect to a reverse-only friend comes up before the reverse client has
// published the friend's outbound. The tunnel runs on the fallback meanwhile,
// but the saved selection stays the friend's so the outbound restores it.
func TestReverseConnectKeepsTheFriendUntilItsOutboundIsUp(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	tun := &fakeHelperTunnel{friendless: true, refuseFriend: true}
	tun.attach(r)
	require.NoError(t, r.srvManager.AddServers(directServers("lantern-1"), false))
	r.reverseFriends = &reverseFriends{byPairing: map[string]string{pairingHash(reversePairing): ppFriendB}}
	friend := vpn.PersonalProxySelection(ppFriendB)
	r.persistSelection(friend)
	evts := fallbackEvents(t)

	require.NoError(t, r.ConnectVPN(t.Context(), friend))
	assert.Equal(t, friend, selectedTag(t), "the fallback was saved over the friend")
	assert.NotEqual(t, friend, tun.current())
	assert.Empty(t, evts)

	tun.mu.Lock()
	tun.refuseFriend = false
	tun.mu.Unlock()
	require.NoError(t, reverseBox{r: r}.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendB)}))
	assert.Equal(t, friend, tun.current())
	assert.Equal(t, friend, selectedTag(t))
}
