package unbounded

import (
	"crypto/ed25519"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/broflake/clientcore"
	"github.com/getlantern/broflake/common/helperticket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
)

// fakeTickets is a settable ticket source. lapsed hides the tickets from
// Volunteer, as expiry does, while AnsweredVolunteer still returns them.
type fakeTickets struct {
	mu     sync.Mutex
	v      []string
	lapsed bool
}

func (f *fakeTickets) Volunteer() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lapsed {
		return nil
	}
	return slices.Clone(f.v)
}

func (f *fakeTickets) AnsweredVolunteer() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.v)
}

func (f *fakeTickets) lapse() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lapsed = true
}

func (f *fakeTickets) set(v ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v = v
}

func volunteerTicket(t *testing.T, priv ed25519.PrivateKey, pairing string) string {
	t.Helper()
	tk, err := helperticket.Sign(priv, helperticket.Ticket{PairingID: pairing, Role: helperticket.RoleVolunteer, Expires: time.Now().Add(15 * time.Minute)})
	require.NoError(t, err)
	return tk
}

// recordingWidgets resets the manager with a factory that records each
// widget and its WebRTC options, and stops the manager at cleanup.
func recordingWidgets(t *testing.T) (count *atomic.Int32, opts func(i int) *clientcore.WebRTCOptions, widgets func(i int) *fakeWidget) {
	t.Helper()
	var mu sync.Mutex
	var gotOpts []*clientcore.WebRTCOptions
	var gotWidgets []*fakeWidget
	count = &atomic.Int32{}
	resetManager(t, func(_ *clientcore.BroflakeOptions, rtc *clientcore.WebRTCOptions, _ *clientcore.EgressOptions) (widget, error) {
		w := &fakeWidget{}
		mu.Lock()
		gotOpts = append(gotOpts, rtc)
		gotWidgets = append(gotWidgets, w)
		mu.Unlock()
		count.Add(1)
		return w, nil
	})
	t.Cleanup(func() { manager.stop() })
	opts = func(i int) *clientcore.WebRTCOptions { mu.Lock(); defer mu.Unlock(); return gotOpts[i] }
	widgets = func(i int) *fakeWidget { mu.Lock(); defer mu.Unlock(); return gotWidgets[i] }
	return count, opts, widgets
}

func fastRoomSync(t *testing.T) {
	t.Helper()
	prev := roomSyncInterval
	roomSyncInterval = 10 * time.Millisecond
	t.Cleanup(func() { roomSyncInterval = prev })
}

const (
	pairingA = "6f1c2b0e-8a7d-4c1e-9f00-0123456789ab"
	pairingB = "0b6f3a52-2d4e-4f7a-8c1d-9e0f1a2b3c4d"
	pairingC = "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
)

func TestWidgetCarriesVolunteerTicketsAndServesThePoolByDefault(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	count, opts, _ := recordingWidgets(t)
	src := &fakeTickets{}
	src.set(volunteerTicket(t, priv, pairingA))
	SetTicketSource(src)
	primeManager(t, nil)

	manager.start()
	waitForCount(t, count, 1, 2*time.Second)
	assert.Equal(t, src.Volunteer(), opts(0).CurrentRoomTickets())
	assert.True(t, opts(0).ServesPool(), "help others too is on by default")
}

func TestWidgetWithoutTicketSourceJoinsNoRooms(t *testing.T) {
	count, opts, _ := recordingWidgets(t)
	primeManager(t, nil)
	manager.start()
	waitForCount(t, count, 1, 2*time.Second)
	assert.Empty(t, opts(0).CurrentRoomTickets())
	assert.True(t, opts(0).ServesPool())
}

func TestWidgetFollowsTheServePoolSetting(t *testing.T) {
	fastRoomSync(t)
	require.NoError(t, settings.Set(settings.PeerServePoolKey, false))
	t.Cleanup(func() { _ = settings.Set(settings.PeerServePoolKey, true) })
	count, opts, _ := recordingWidgets(t)
	SetTicketSource(&fakeTickets{})
	primeManager(t, nil)

	manager.start()
	waitForCount(t, count, 1, 2*time.Second)
	assert.False(t, opts(0).ServesPool(), "help others too off: rooms only")

	require.NoError(t, settings.Set(settings.PeerServePoolKey, true))
	require.Eventually(t, func() bool { return opts(0).ServesPool() }, 2*time.Second, 10*time.Millisecond,
		"a running widget picks the setting up without a restart")
	assert.EqualValues(t, 1, count.Load())
}

// Renewed tickets reach the running widget; a new pairing needs no restart;
// an ended pairing restarts the widget, ending its sessions.
func TestWidgetRestartsWhenAPairingEnds(t *testing.T) {
	fastRoomSync(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	count, opts, widgets := recordingWidgets(t)
	src := &fakeTickets{}
	a, b := volunteerTicket(t, priv, pairingA), volunteerTicket(t, priv, pairingB)
	src.set(a, b)
	SetTicketSource(src)
	primeManager(t, nil)
	manager.start()
	waitForCount(t, count, 1, 2*time.Second)

	renewedA, c := volunteerTicket(t, priv, pairingA), volunteerTicket(t, priv, pairingC)
	src.set(renewedA, b, c)
	require.Eventually(t, func() bool { return slices.Equal(opts(0).CurrentRoomTickets(), []string{renewedA, b, c}) },
		2*time.Second, 10*time.Millisecond, "renewed and added tickets reach the running widget")
	assert.Never(t, func() bool { return count.Load() > 1 }, 100*time.Millisecond, 10*time.Millisecond,
		"renewing or adding a pairing must not restart the widget")

	src.set(b, c)
	waitForCount(t, count, 2, 2*time.Second)
	assert.EqualValues(t, 1, widgets(0).stopCalled.Load(), "the old widget, and its sessions, stopped")
	assert.Equal(t, []string{b, c}, opts(1).CurrentRoomTickets())
	assert.Never(t, func() bool { return count.Load() > 2 }, 100*time.Millisecond, 10*time.Millisecond,
		"one ended pairing is one restart")
}

// Tickets that lapse because lantern-cloud is unreachable end no pairing, so
// the widget keeps its open sessions.
func TestWidgetKeepsRunningWhenTicketsOnlyLapse(t *testing.T) {
	fastRoomSync(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	count, opts, _ := recordingWidgets(t)
	src := &fakeTickets{}
	src.set(volunteerTicket(t, priv, pairingA))
	SetTicketSource(src)
	primeManager(t, nil)
	manager.start()
	waitForCount(t, count, 1, 2*time.Second)

	src.lapse()
	require.Eventually(t, func() bool { return len(opts(0).CurrentRoomTickets()) == 0 },
		2*time.Second, 10*time.Millisecond, "lapsed tickets leave the wire")
	assert.Never(t, func() bool { return count.Load() > 1 }, 100*time.Millisecond, 10*time.Millisecond,
		"lapsed tickets must not restart the widget")
}

func TestVolunteerPairings(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	got := volunteerPairings([]string{volunteerTicket(t, priv, pairingA), volunteerTicket(t, priv, pairingA), "junk"})
	assert.Equal(t, map[string]bool{pairingA: true}, got)
}

// Tickets reach a new widget at its next genesis message, not the next sync.
func TestWidgetReadsFreshTicketsWithoutWaitingForASync(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	count, opts, _ := recordingWidgets(t)
	src := &fakeTickets{}
	SetTicketSource(src)
	primeManager(t, nil)
	manager.start()
	waitForCount(t, count, 1, 2*time.Second)
	require.Empty(t, opts(0).CurrentRoomTickets())

	a := volunteerTicket(t, priv, pairingA)
	src.set(a)
	assert.Equal(t, []string{a}, opts(0).CurrentRoomTickets())
}

// Turning "help others too" off ends sessions already open with strangers.
func TestWidgetRestartsWhenServePoolTurnsOff(t *testing.T) {
	fastRoomSync(t)
	require.NoError(t, settings.Set(settings.PeerServePoolKey, true))
	t.Cleanup(func() { _ = settings.Set(settings.PeerServePoolKey, true) })
	count, opts, widgets := recordingWidgets(t)
	SetTicketSource(&fakeTickets{})
	primeManager(t, nil)
	manager.start()
	waitForCount(t, count, 1, 2*time.Second)
	require.True(t, opts(0).ServesPool())

	require.NoError(t, settings.Set(settings.PeerServePoolKey, false))
	waitForCount(t, count, 2, 2*time.Second)
	assert.EqualValues(t, 1, widgets(0).stopCalled.Load(), "the old widget, and its sessions, stopped")
	assert.False(t, opts(1).ServesPool())
	assert.Never(t, func() bool { return count.Load() > 2 }, 100*time.Millisecond, 10*time.Millisecond,
		"one flip is one restart")
}

func TestRestartIfCurrentIgnoresStaleRuns(t *testing.T) {
	count, _, widgets := recordingWidgets(t)
	primeManager(t, nil)
	manager.start()
	waitForCount(t, count, 1, 2*time.Second)

	manager.restartIfCurrent(make(chan struct{}))
	manager.restartIfCurrent(nil)
	assert.EqualValues(t, 1, count.Load(), "a stale run is not restarted")
	assert.Zero(t, widgets(0).stopCalled.Load())

	manager.mu.Lock()
	done := manager.done
	manager.mu.Unlock()
	manager.restartIfCurrent(done)
	waitForCount(t, count, 2, 2*time.Second)
	assert.EqualValues(t, 1, widgets(0).stopCalled.Load())

	manager.restartIfCurrent(done)
	assert.Never(t, func() bool { return count.Load() > 2 }, 50*time.Millisecond, 5*time.Millisecond,
		"the replaced run's done no longer restarts anything")
}
