package backend

import (
	"bytes"
	"context"
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

// A friend reachable only by its reverse outbound stays selected while the VPN
// is down, and is reachable again once the reconnected client restores the
// outbound. Only the friend's pairing ending clears the selection.
func TestReverseKeepsASelectionAcrossAVPNDisconnect(t *testing.T) {
	r, fake := newUnstartedReverseTestBackend(t, t.TempDir(), false)
	box := reverseBox{r: r}
	fake.box, fake.tags = box, []string{lbA.ReverseHelperTag(ppFriendB)}
	r.startReverse()
	friend := vpn.PersonalProxySelection(ppFriendB)

	emitReverseFeature(true)
	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
	r.persistSelection(friend)

	emitVPNStatus(vpn.Disconnected)
	require.Eventually(t, notRunning(fake), time.Second, 5*time.Millisecond)
	assert.Empty(t, r.reverseOutbounds())
	assert.Equal(t, friend, selectedTag(t))

	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
	assert.Equal(t, friend, selectedTag(t))
	assert.NoError(t, r.checkConnectSelection(friend))

	require.NoError(t, box.SetReversePortal(nil, ""), "the last inviter pairing ended")
	assert.Equal(t, vpn.AutoSelectTag, selectedTag(t))
}

func TestReverseBoxClearsASelectionWhenItsPairingEnds(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	box := reverseBox{r: r}
	friend := vpn.PersonalProxySelection(ppFriendB)

	require.NoError(t, box.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendA), lbA.ReverseHelperTag(ppFriendB)}))
	r.persistSelection(friend)
	require.NoError(t, box.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendA)}))
	assert.Equal(t, vpn.PersonalProxiesTag, selectedTag(t), "the group remains")

	require.NoError(t, box.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendB)}))
	r.persistSelection(friend)
	require.NoError(t, box.SetReversePortal(nil, ""))
	assert.Equal(t, vpn.AutoSelectTag, selectedTag(t))
}

// An empty set means the client has nothing to offer for now, such as no
// address to reach it at, not that a pairing ended.
func TestReverseBoxKeepsASelectionThroughAnEmptySet(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	box := reverseBox{r: r}
	friend := vpn.PersonalProxySelection(ppFriendB)

	require.NoError(t, box.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendB)}))
	r.persistSelection(friend)
	require.NoError(t, box.SetReverseOutbounds(nil))
	assert.Equal(t, friend, selectedTag(t))
}
