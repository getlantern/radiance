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
	"github.com/getlantern/radiance/vpn"
)

type fakeReverse struct {
	starts, stops, networks, pairings atomic.Int32
	running                           atomic.Bool
}

func (f *fakeReverse) Start(context.Context) error {
	f.starts.Add(1)
	f.running.Store(true)
	return nil
}

func (f *fakeReverse) Stop(context.Context) {
	f.stops.Add(1)
	f.running.Store(false)
}

func (f *fakeReverse) NetworkChanged()  { f.networks.Add(1) }
func (f *fakeReverse) PairingsChanged() { f.pairings.Add(1) }

const reverseSettle = 150 * time.Millisecond

func newReverseTestBackend(t *testing.T, unsupported bool) (*LocalBackend, *fakeReverse) {
	t.Helper()
	prev := reverseUnsupported
	reverseUnsupported = func() bool { return unsupported }
	t.Cleanup(func() { reverseUnsupported = prev })
	r := newPersonalProxiesBackend(t)
	r.helperPairingKeys = &helper.PairingKeys{}
	fake := &fakeReverse{}
	r.useReverseClient(fake)
	r.startReverse()
	t.Cleanup(r.stopReverse)
	return r, fake
}

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
	require.Eventually(t, func() bool { return !fake.running.Load() }, time.Second, 5*time.Millisecond)

	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)

	emitReverseFeature(false)
	require.Eventually(t, func() bool { return !fake.running.Load() }, time.Second, 5*time.Millisecond)
	assert.Equal(t, int32(2), fake.starts.Load())
	assert.Equal(t, int32(2), fake.stops.Load())
}

func TestReverseStopsOnAnErrorStatus(t *testing.T) {
	_, fake := newReverseTestBackend(t, false)
	emitReverseFeature(true)
	emitVPNStatus(vpn.Connected)
	require.Eventually(t, fake.running.Load, time.Second, 5*time.Millisecond)
	emitVPNStatus(vpn.ErrorStatus)
	require.Eventually(t, func() bool { return !fake.running.Load() }, time.Second, 5*time.Millisecond)
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

// A selection of a friend reachable only through its reverse outbound falls
// back once the reverse client withdraws that outbound.
func TestReverseBoxClearsASelectionItWithdraws(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	box := reverseBox{r: r}
	friend := vpn.PersonalProxySelection(ppFriendB)

	require.NoError(t, box.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendB)}))
	r.persistSelection(friend)
	require.Equal(t, friend, selectedTag(t))
	require.NoError(t, box.SetReverseOutbounds(nil))
	assert.Equal(t, vpn.AutoSelectTag, selectedTag(t))

	require.NoError(t, box.SetReverseOutbounds([]string{lbA.ReverseHelperTag(ppFriendB)}))
	r.persistSelection(friend)
	require.Equal(t, friend, selectedTag(t))
	require.NoError(t, box.SetReversePortal(nil, ""))
	assert.Equal(t, vpn.AutoSelectTag, selectedTag(t))
}
