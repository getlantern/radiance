package backend

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/getlantern/broflake/common/helperticket"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

const (
	ppFriendA  = "helper-aaaaaaaaaaaa"
	ppFriendB  = "helper-bbbbbbbbbbbb"
	ppConsumer = "helper-0000000000ff"
	ppPairingA = "6f1c2b0e-8a7d-4c1e-9f00-0123456789aa"
	ppPairingB = "6f1c2b0e-8a7d-4c1e-9f00-0123456789bb"
)

func consumerServers() servers.ServerList {
	return servers.ServerList{Servers: []*servers.Server{{
		Tag: ppConsumer, Type: "unbounded", IsLantern: true,
		Options: option.Outbound{Tag: ppConsumer, Type: "unbounded", Options: &lbO.UnboundedOutboundOptions{}},
	}}}
}

// fakeTickets is the ticket service's answer, which a test changes.
type fakeTickets struct {
	mu         sync.Mutex
	tickets    helper.Tickets
	reconciled chan struct{}
}

func (f *fakeTickets) fetch(context.Context) (helper.Tickets, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tickets, nil
}

// issue sets the answer to one inviter ticket per pairing, keyed by tag.
func (f *fakeTickets) issue(t *testing.T, pairings map[string]string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	tk := helper.Tickets{Lifetime: 15 * time.Minute}
	if len(pairings) > 0 {
		tk.InviterPairings = pairings
	}
	for _, pairing := range pairings {
		s, err := helperticket.Sign(priv, helperticket.Ticket{PairingID: pairing, Role: helperticket.RoleInviter, Expires: time.Now().Add(15 * time.Minute)})
		require.NoError(t, err)
		tk.Inviter = append(tk.Inviter, s)
	}
	f.mu.Lock()
	f.tickets = tk
	f.mu.Unlock()
}

// withTickets gives r a ticket source answering from the returned fake, as
// NewLocalBackend wires it, and waits for the reconcile its first answer runs.
func withTickets(t *testing.T, r *LocalBackend, pairings map[string]string) *fakeTickets {
	t.Helper()
	f := &fakeTickets{reconciled: make(chan struct{}, 16)}
	f.issue(t, pairings)
	r.helperTickets = helper.NewSource(r.ctx, f.fetch)
	// A reconcile still running after the test would race the next test's
	// setup (both reach sing-box's registry), so cleanup waits for it.
	var (
		mu      sync.Mutex
		stopped bool
		running sync.WaitGroup
	)
	r.helperTickets.SetOnInviterChange(func() {
		mu.Lock()
		if stopped {
			mu.Unlock()
			return
		}
		running.Add(1)
		mu.Unlock()
		defer running.Done()
		r.reconcilePersonalProxy()
		f.reconciled <- struct{}{}
	})
	t.Cleanup(func() {
		mu.Lock()
		stopped = true
		mu.Unlock()
		running.Wait()
	})
	r.helperTickets.Inviter()
	f.awaitReconcile(t)
	return f
}

// reissue changes the tickets and waits for the reconcile the change runs,
// which reads settings through sing-box's registry and so must not overlap
// the test's own reads.
func (f *fakeTickets) reissue(t *testing.T, r *LocalBackend, pairings map[string]string) {
	t.Helper()
	f.issue(t, pairings)
	r.helperTickets.Invalidate()
	r.helperTickets.Inviter()
	f.awaitReconcile(t)
}

func (f *fakeTickets) awaitReconcile(t *testing.T) {
	t.Helper()
	select {
	case <-f.reconciled:
	case <-time.After(2 * time.Second):
		t.Fatal("no reconcile after the tickets changed")
	}
}

func fallbackEvents(t *testing.T) <-chan vpn.PersonalProxyFallbackEvent {
	ch := make(chan vpn.PersonalProxyFallbackEvent, 8)
	sub := events.Subscribe(func(evt vpn.PersonalProxyFallbackEvent) { ch <- evt })
	t.Cleanup(sub.Unsubscribe)
	return ch
}

func requireFallbackEvent(t *testing.T, ch <-chan vpn.PersonalProxyFallbackEvent, want vpn.PersonalProxyFallbackEvent) {
	t.Helper()
	select {
	case got := <-ch:
		assert.Equal(t, want.Tag, got.Tag)
		assert.Equal(t, want.Fallback, got.Fallback)
	case <-time.After(2 * time.Second):
		t.Fatal("no fallback event")
	}
}

func selectedTag(t *testing.T) string {
	t.Helper()
	if settings.GetBool(settings.AutoConnectKey) {
		return vpn.AutoSelectTag
	}
	var sel servers.Server
	require.NoError(t, settings.GetStruct(settings.SelectedServerKey, &sel))
	return sel.Tag
}

// A friend's selection is persisted as its selection tag and type alone, and
// read back as long as the friend has a member.
func TestPersonalProxySelectionPersistsTheTagOnly(t *testing.T) {
	dataDir := t.TempDir()
	r := newPersonalProxiesBackendIn(t, dataDir)
	withTickets(t, r, map[string]string{ppFriendA: ppPairingA})
	require.NoError(t, r.srvManager.AddServers(directServers(ppFriendA), false))
	sel := vpn.PersonalProxySelection(ppFriendA)
	r.persistSelection(sel)

	assert.Equal(t, sel, selectedTag(t))
	raw, err := json.Marshal(settings.Get(settings.SelectedServerKey))
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal(raw, &stored))
	assert.Equal(t, sel, stored["tag"])
	assert.Equal(t, "mutableurltest", stored["type"])
	for k, v := range stored {
		if k != "tag" && k != "type" {
			assert.Empty(t, v, "only the tag and type are persisted: %s", k)
		}
	}
	file, err := os.ReadFile(filepath.Join(dataDir, "settings.json"))
	require.NoError(t, err)
	assert.Contains(t, string(file), sel)
	assert.NotContains(t, string(file), ppPairingA, "never a pairing ID")

	got, exists, err := r.SelectedServer()
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, sel, got.Tag)
	for _, s := range r.AllServers() {
		assert.NotEqual(t, sel, s.Tag, "never listed among servers")
	}
}

// Selecting a friend needs a live member: its direct outbound, or a rooms
// consumer with a ticket for its pairing.
func TestPersonalProxySelectionNeedsALiveMember(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	tun := &fakeHelperTunnel{}
	tun.attach(r)
	withTickets(t, r, map[string]string{ppFriendB: ppPairingB})
	require.NoError(t, r.srvManager.AddServers(directServers(ppFriendA), false))

	assert.True(t, r.friendReach(ppFriendA).selectable(), "direct")
	assert.False(t, r.friendReach(ppFriendB).selectable(), "a ticket without a consumer")
	require.ErrorIs(t, r.SelectServer(vpn.PersonalProxySelection(ppFriendB)), vpn.ErrPersonalProxyUnavailable)
	require.ErrorIs(t, r.SelectServer(vpn.PersonalProxySelection("helper-cccccccccccc")), vpn.ErrPersonalProxyUnavailable)
	require.Error(t, r.SelectServer(vpn.PersonalProxyTag), "the group itself is not selectable by its tag")

	require.NoError(t, r.srvManager.AddServers(consumerServers(), false))
	assert.True(t, r.friendReach(ppFriendB).selectable(), "rooms only")
}

// Before the ticket source first answers, a persisted rooms-only selection
// stands, but a new one is refused.
func TestPersonalProxySelectionSurvivesUnknownTickets(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	require.NoError(t, r.srvManager.AddServers(consumerServers(), false))
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	r.helperTickets = helper.NewSource(r.ctx, func(ctx context.Context) (helper.Tickets, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return helper.Tickets{}, ctx.Err()
	})
	f := r.friendReach(ppFriendA)
	assert.True(t, f.live())
	assert.False(t, f.selectable())
}

// A friend whose direct outbound goes keeps the selection while a rooms
// ticket remains; when the last member goes the selection falls back to the
// personal proxies group, and then, with no helpers left, to auto-select.
func TestPersonalProxySelectionFallsBackWhenItsLastMemberGoes(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	tun := &fakeHelperTunnel{}
	tun.attach(r)
	tun.up()
	tickets := withTickets(t, r, map[string]string{ppFriendA: ppPairingA, ppFriendB: ppPairingB})
	evts := fallbackEvents(t)
	all := directServers(ppFriendA, ppFriendB)
	all.Servers = append(all.Servers, consumerServers().Servers...)
	require.NoError(t, r.updateServers(all, true))
	sel := vpn.PersonalProxySelection(ppFriendA)
	r.persistSelection(sel)

	// Friend A goes offline: its direct route goes, its ticket stays.
	rest := directServers(ppFriendB)
	rest.Servers = append(rest.Servers, consumerServers().Servers...)
	require.NoError(t, r.updateServers(rest, true))
	assert.Equal(t, sel, selectedTag(t), "the group shrinks; the selection stays")
	assert.Empty(t, evts)

	// Its pairing ends: the ticket goes too.
	tickets.reissue(t, r, map[string]string{ppFriendB: ppPairingB})
	requireFallbackEvent(t, evts, vpn.PersonalProxyFallbackEvent{Tag: ppFriendA, Fallback: vpn.PersonalProxiesTag})
	assert.Equal(t, vpn.PersonalProxiesTag, selectedTag(t))
	assert.Contains(t, tun.callLog(), "select:"+vpn.PersonalProxiesTag)

	// With no helper left, a friend's selection falls back to auto-select.
	require.NoError(t, r.updateServers(directServers(ppFriendB), true))
	r.persistSelection(vpn.PersonalProxySelection(ppFriendB))
	require.NoError(t, r.updateServers(directServers("lantern-1"), true))
	requireFallbackEvent(t, evts, vpn.PersonalProxyFallbackEvent{Tag: ppFriendB, Fallback: vpn.AutoSelectTag})
	assert.Equal(t, vpn.AutoSelectTag, selectedTag(t))
}

// A ticket-key rotation changes every tag at once: the selected tag vanishes
// from the config and the tickets, and the selection falls back.
func TestPersonalProxySelectionFallsBackWhenItsTagIsRotated(t *testing.T) {
	const rotatedA, rotatedB = "helper-abababababab", "helper-bcbcbcbcbcbc"
	r := newPersonalProxiesBackend(t)
	tickets := withTickets(t, r, map[string]string{ppFriendA: ppPairingA, ppFriendB: ppPairingB})
	evts := fallbackEvents(t)
	before := directServers(ppFriendA, ppFriendB)
	before.Servers = append(before.Servers, consumerServers().Servers...)
	require.NoError(t, r.updateServers(before, true))
	r.persistSelection(vpn.PersonalProxySelection(ppFriendA))

	tickets.reissue(t, r, map[string]string{rotatedA: ppPairingA, rotatedB: ppPairingB})
	after := directServers(rotatedA, rotatedB)
	after.Servers = append(after.Servers, consumerServers().Servers...)
	require.NoError(t, r.updateServers(after, true))
	requireFallbackEvent(t, evts, vpn.PersonalProxyFallbackEvent{Tag: ppFriendA, Fallback: vpn.PersonalProxiesTag})
	assert.Equal(t, vpn.PersonalProxiesTag, selectedTag(t))
}

// Other selections still fall back straight to auto-select, without an event.
func TestServerSelectionStillFallsBackToAuto(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	evts := fallbackEvents(t)
	require.NoError(t, r.updateServers(directServers("lantern-1", ppFriendA), true))
	r.persistSelection("lantern-1")
	_, err := r.srvManager.RemoveServers([]string{"lantern-1"})
	require.NoError(t, err)
	r.clearSelectedIfMissing()
	assert.Equal(t, vpn.AutoSelectTag, selectedTag(t))
	assert.Empty(t, evts)
}

func TestPersonalProxyStatus(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	assert.Empty(t, r.PersonalProxyStatus())
	withTickets(t, r, map[string]string{ppFriendA: ppPairingA, ppFriendB: ppPairingB})
	require.NoError(t, r.srvManager.AddServers(directServers(ppFriendA, "lantern-1"), false))
	assert.Equal(t, []vpn.PersonalProxyStatus{
		{Tag: ppFriendA, Direct: true},
		{Tag: ppFriendB},
	}, r.PersonalProxyStatus(), "no consumer, no rooms")

	require.NoError(t, r.srvManager.AddServers(consumerServers(), false))
	assert.Equal(t, []vpn.PersonalProxyStatus{
		{Tag: ppFriendA, Direct: true, Rooms: true},
		{Tag: ppFriendB, Rooms: true},
	}, r.PersonalProxyStatus())
}

// A connect may restore the persisted friend before its tickets are known,
// but a friend it newly chooses must be selectable, as SelectServer requires.
func TestConnectRefusesANewFriendWithoutAMember(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	require.NoError(t, r.srvManager.AddServers(consumerServers(), false))
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	r.helperTickets = helper.NewSource(r.ctx, func(ctx context.Context) (helper.Tickets, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return helper.Tickets{}, ctx.Err()
	})
	persisted, fresh := vpn.PersonalProxySelection(ppFriendA), vpn.PersonalProxySelection(ppFriendB)
	r.persistSelection(persisted)
	require.Equal(t, persisted, selectedTag(t))

	require.ErrorIs(t, r.ConnectVPN(t.Context(), fresh), vpn.ErrPersonalProxyUnavailable)
	assert.NoError(t, r.checkConnectSelection(persisted), "the persisted friend is restored")
}

// selectionSaves records, for each SelectionSavedEvent, the selection a
// settings read saw when the event was handled.
func selectionSaves(t *testing.T) <-chan string {
	ch := make(chan string, 16)
	sub := events.Subscribe(func(vpn.SelectionSavedEvent) {
		if settings.GetBool(settings.AutoConnectKey) {
			ch <- vpn.AutoSelectTag
			return
		}
		var sel servers.Server
		_ = settings.GetStruct(settings.SelectedServerKey, &sel)
		ch <- sel.Tag
	})
	t.Cleanup(sub.Unsubscribe)
	return ch
}

func requireSaved(t *testing.T, saves <-chan string, want string) {
	t.Helper()
	select {
	case got := <-saves:
		assert.Equal(t, want, got, "the event follows the save")
	case <-time.After(2 * time.Second):
		t.Fatal("no selection saved event")
	}
}

func requireNoSave(t *testing.T, saves <-chan string) {
	t.Helper()
	select {
	case got := <-saves:
		t.Fatalf("selection saved event for %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// Every path that saves the selection announces it once the save is
// readable: a select, a connect, a fallback and a settings patch.
func TestSelectionSavedEvents(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	tun := &fakeHelperTunnel{}
	tun.attach(r)
	tun.up()
	require.NoError(t, r.updateServers(directServers("lantern-1", "lantern-2", ppFriendA), true))
	saves := selectionSaves(t)

	require.NoError(t, r.SelectServer("lantern-1"))
	requireSaved(t, saves, "lantern-1")

	r.persistConnectedSelection("lantern-2")
	requireSaved(t, saves, "lantern-2")

	r.persistSelection(vpn.PersonalProxySelection(ppFriendA))
	requireSaved(t, saves, vpn.PersonalProxySelection(ppFriendA))
	require.NoError(t, r.updateServers(directServers("lantern-1", "lantern-2"), true))
	requireSaved(t, saves, vpn.AutoSelectTag)

	require.NoError(t, r.SelectServer("lantern-1"))
	requireSaved(t, saves, "lantern-1")
	require.NoError(t, r.PatchSettings(settings.Settings{settings.AutoConnectKey: true}))
	requireSaved(t, saves, vpn.AutoSelectTag)
}

// A selection that is refused, or whose save fails, announces nothing.
func TestSelectionSavedEventsOnlyAfterASave(t *testing.T) {
	dataDir := t.TempDir()
	r := newPersonalProxiesBackendIn(t, dataDir)
	tun := &fakeHelperTunnel{}
	tun.attach(r)
	tun.up()
	require.NoError(t, r.updateServers(directServers("lantern-1"), true))
	saves := selectionSaves(t)

	require.Error(t, r.SelectServer("lantern-unknown"))
	requireNoSave(t, saves)

	require.NoError(t, os.Chmod(dataDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o700) })
	r.persistSelection("lantern-1")
	requireNoSave(t, saves)
}

// closableBackend is a personal proxies backend that Close can tear down.
func closableBackend(t *testing.T) *LocalBackend {
	t.Helper()
	r := newPersonalProxiesBackend(t)
	ctx, cancel := context.WithCancel(r.ctx)
	r.ctx, r.cancel = ctx, cancel
	return r
}

// A ticket-change reconcile delayed until after Close must not touch the
// saved selection: on mobile a replacement backend may own it by then.
func TestTicketReconcileStopsAtClose(t *testing.T) {
	t.Run("after close", func(t *testing.T) {
		r := closableBackend(t)
		sel := vpn.PersonalProxySelection(ppFriendA)
		require.NoError(t, r.srvManager.AddServers(directServers(ppFriendA), false))
		r.persistSelection(sel)
		_, err := r.srvManager.RemoveServers([]string{ppFriendA})
		require.NoError(t, err)
		r.Close()
		r.reconcilePersonalProxy()
		assert.Equal(t, sel, selectedTag(t), "a closed backend fell back")
	})
	t.Run("in flight", func(t *testing.T) {
		r := closableBackend(t)
		r.selectionMu.Lock() // a reconcile in flight
		closed := make(chan struct{})
		go func() { r.Close(); close(closed) }()
		select {
		case <-closed:
			r.selectionMu.Unlock()
			t.Fatal("Close returned while a reconcile held the selection")
		case <-time.After(200 * time.Millisecond):
		}
		r.selectionMu.Unlock()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("Close never returned")
		}
	})
}

// A rooms-only friend whose last ticket goes while the tunnel switches to it
// is still saved as the selection the tunnel made; the reconcile the ticket
// change runs then falls back from it.
func TestSelectServerSavesTheFriendItSwitchedTo(t *testing.T) {
	r := newPersonalProxiesBackend(t)
	tun := &fakeHelperTunnel{}
	tun.attach(r)
	tun.up()
	list := directServers("lantern-1")
	list.Servers = append(list.Servers, consumerServers().Servers...)
	require.NoError(t, r.updateServers(list, true))
	require.NoError(t, r.SelectServer("lantern-1"))
	f := &fakeTickets{}
	f.issue(t, map[string]string{ppFriendA: ppPairingA})
	r.helperTickets = helper.NewSource(r.ctx, f.fetch)
	require.Eventually(t, func() bool { tk, _ := r.helperTickets.InviterFor(ppFriendA); return len(tk) > 0 },
		2*time.Second, 5*time.Millisecond)

	tun.onSelect = func() {
		tun.onSelect = nil
		f.issue(t, nil)
		r.helperTickets.Invalidate()
		require.Eventually(t, func() bool { tk, _ := r.helperTickets.InviterFor(ppFriendA); return len(tk) == 0 },
			2*time.Second, 5*time.Millisecond)
	}
	sel := vpn.PersonalProxySelection(ppFriendA)
	require.NoError(t, r.SelectServer(sel))
	assert.Equal(t, sel, selectedTag(t), "the switch was made but not saved")
}
