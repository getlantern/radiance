package vpn

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/servers"
)

const (
	friendA  = "helper-aaaaaaaaaaaa"
	friendB  = "helper-bbbbbbbbbbbb"
	friendC  = "helper-cccccccccccc"
	consumer = "helper-0000000000ff"
)

// fakeInviterTickets holds inviter tickets by friend tag.
type fakeInviterTickets struct {
	mu         sync.Mutex
	byFriend   map[string][]string
	unanswered bool
}

func (f *fakeInviterTickets) Inviter() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, friend := range slices.Sorted(maps.Keys(f.byFriend)) {
		all = append(all, f.byFriend[friend]...)
	}
	return all
}

func (f *fakeInviterTickets) InviterFor(tag string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.byFriend[tag]), !f.unanswered
}

func (f *fakeInviterTickets) set(friend string, tickets ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(tickets) == 0 {
		delete(f.byFriend, friend)
		return
	}
	f.byFriend[friend] = tickets
}

// standInRoomsConsumer makes SOCKS outbounds stand in for the rooms consumer
// for the rest of the test.
func standInRoomsConsumer(t *testing.T) {
	t.Helper()
	prev := roomsConsumerType
	roomsConsumerType = C.TypeSOCKS
	t.Cleanup(func() { roomsConsumerType = prev })
}

func consumerServer(tag string) *servers.Server {
	return &servers.Server{Tag: tag, Type: C.TypeSOCKS, IsLantern: true,
		Options: O.Outbound{Tag: tag, Type: C.TypeSOCKS, Options: &O.SOCKSOutboundOptions{
			ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: 1},
		}}}
}

// startPersonalProxyTestTunnel starts a tunnel with lantern-1, the direct
// helpers in friends and a rooms consumer, whose tickets come from the
// returned source: one per friend in friends.
func startPersonalProxyTestTunnel(t *testing.T, friends ...string) (*tunnel, *fakeInviterTickets) {
	t.Helper()
	standInRoomsConsumer(t)
	tun := startPersonalProxiesTestTunnel(t)
	tickets := &fakeInviterTickets{byFriend: map[string][]string{}}
	tun.inviterTickets = tickets
	list := servers.ServerList{Servers: []*servers.Server{consumerServer(consumer)}}
	for _, f := range friends {
		list.Servers = append(list.Servers, directServer(f))
		tickets.set(f, "ticket-"+f)
	}
	require.NoError(t, tun.addOutbounds(list))
	return tun, tickets
}

func personalProxyMembers(tun *tunnel) []string {
	g := tun.personalProxyGroup()
	if g == nil {
		return nil
	}
	return slices.Clone(g.All())
}

// awaitFirstProbe waits for the personal proxy group's first URL test to
// reach tag: lantern-box's url-test group reads its member list unlocked as a
// check starts (a race with Remove), and every group checks as it starts.
func awaitFirstProbe(t *testing.T, tun *tunnel, tag string) {
	t.Helper()
	history := tun.clashServer.HistoryStorage()
	require.Eventually(t, func() bool { return history.LoadURLTestHistory(tag) != nil },
		5*time.Second, 5*time.Millisecond)
}

func selectedInManual(t *testing.T, tun *tunnel) string {
	t.Helper()
	sel, ok := tun.mutGrpMgr.OutboundGroup(ManualSelectTag)
	require.True(t, ok)
	return sel.Now()
}

func TestPersonalProxySelectionParsing(t *testing.T) {
	friend, ok := ParsePersonalProxySelection(PersonalProxySelection(friendA))
	assert.True(t, ok)
	assert.Equal(t, friendA, friend)
	for _, tag := range []string{friendA, PersonalProxyTag, PersonalProxyTag + ":", PersonalProxyTag + ":lantern-1", PersonalProxiesTag} {
		_, ok := ParsePersonalProxySelection(tag)
		assert.False(t, ok, tag)
	}
	assert.Contains(t, ReservedTags(), PersonalProxyTag)
}

// Selecting a friend creates its group and narrows the rooms tickets to it;
// switching friends rebuilds both; moving the selection elsewhere removes the
// group and restores every ticket. Each change restarts the rooms consumer.
func TestPersonalProxyGroupFollowsSelection(t *testing.T) {
	tun, _ := startPersonalProxyTestTunnel(t, friendA, friendB)
	assert.Nil(t, tun.personalProxyGroup(), "no selection, no group")
	assert.Equal(t, []string{"ticket-" + friendA, "ticket-" + friendB}, tun.roomTickets())
	consumerOut := tun.liveOutbound(consumer)

	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	assert.ElementsMatch(t, []string{friendA, consumer}, personalProxyMembers(tun))
	assert.Equal(t, PersonalProxyTag, selectedInManual(t, tun))
	assert.Equal(t, ManualSelectTag, tun.clashServer.selectedMode())
	assert.Equal(t, []string{"ticket-" + friendA}, tun.roomTickets(), "tickets for other pairings are filtered")
	assert.NotSame(t, consumerOut, tun.liveOutbound(consumer), "the consumer reconnects narrowed")
	assert.ElementsMatch(t, []string{friendA, friendB, consumer}, personalProxiesMembers(tun),
		"the personal proxies group keeps every helper")
	assert.NotContains(t, selectorMembers(t, tun, AutoSelectTag), PersonalProxyTag)

	consumerOut = tun.liveOutbound(consumer)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	assert.ElementsMatch(t, []string{friendB, consumer}, personalProxyMembers(tun), "at most one group, now B's")
	assert.Equal(t, PersonalProxyTag, selectedInManual(t, tun))
	assert.Equal(t, []string{"ticket-" + friendB}, tun.roomTickets())
	assert.NotSame(t, consumerOut, tun.liveOutbound(consumer))

	consumerOut = tun.liveOutbound(consumer)
	require.NoError(t, tun.selectOutbound("lantern-1"))
	assert.Nil(t, tun.personalProxyGroup(), "deselection removes the group")
	assert.NotContains(t, selectorMembers(t, tun, ManualSelectTag), PersonalProxyTag)
	assert.Equal(t, "lantern-1", selectedInManual(t, tun))
	assert.Equal(t, []string{"ticket-" + friendA, "ticket-" + friendB}, tun.roomTickets(), "narrowing ends")
	assert.NotSame(t, consumerOut, tun.liveOutbound(consumer))

	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	require.NoError(t, tun.selectAuto())
	assert.Nil(t, tun.personalProxyGroup(), "auto-select deselects too")
	assert.Equal(t, AutoSelectTag, tun.clashServer.selectedMode())
	assert.Len(t, tun.roomTickets(), 2)
}

func TestPersonalProxySelectionNeedsAMember(t *testing.T) {
	tun, tickets := startPersonalProxyTestTunnel(t, friendA)
	consumerOut := tun.liveOutbound(consumer)
	require.ErrorIs(t, tun.selectOutbound(PersonalProxySelection(friendC)), ErrPersonalProxyUnavailable)
	require.ErrorIs(t, tun.selectOutbound(PersonalProxyTag), ErrPersonalProxyUnavailable)
	assert.Nil(t, tun.personalProxyGroup())
	assert.Equal(t, AutoSelectTag, tun.clashServer.selectedMode(), "a refused selection changes nothing")
	assert.Same(t, consumerOut, tun.liveOutbound(consumer))

	// A rooms-only friend: a ticket and the consumer, no direct outbound.
	tickets.set(friendC, "ticket-"+friendC)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendC)))
	assert.Equal(t, []string{consumer}, personalProxyMembers(tun))
	assert.Equal(t, []string{"ticket-" + friendC}, tun.roomTickets())
}

// A friend's direct outbound going while its ticket remains shrinks the group
// and keeps the selection; it coming back grows it again.
func TestPersonalProxyGroupShrinksToRooms(t *testing.T) {
	tun, _ := startPersonalProxyTestTunnel(t, friendA, friendB)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	awaitFirstProbe(t, tun, friendA)

	require.NoError(t, tun.removeOutbounds([]string{friendA}))
	assert.Equal(t, []string{consumer}, personalProxyMembers(tun))
	assert.Equal(t, PersonalProxyTag, selectedInManual(t, tun))

	require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer(friendA)}}))
	assert.ElementsMatch(t, []string{friendA, consumer}, personalProxyMembers(tun))
	assert.Equal(t, PersonalProxyTag, selectedInManual(t, tun))
}

// The last member going withdraws the group, and its selection falls back to
// the personal proxies group while it exists.
func TestPersonalProxyGroupLastMemberFallsBackToPersonalProxies(t *testing.T) {
	tun, tickets := startPersonalProxyTestTunnel(t, friendA, friendB)
	tickets.set(friendA)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	require.Equal(t, []string{friendA}, personalProxyMembers(tun))

	require.NoError(t, tun.removeOutbounds([]string{friendA}))
	assert.Nil(t, tun.personalProxyGroup())
	assert.NotContains(t, selectorMembers(t, tun, ManualSelectTag), PersonalProxyTag)
	assert.Equal(t, PersonalProxiesTag, selectedInManual(t, tun))
	assert.Equal(t, ManualSelectTag, tun.clashServer.selectedMode())
	assert.Len(t, tun.roomTickets(), 1, "narrowing ends")
}

// Without the personal proxies group the fallback is auto-select.
func TestPersonalProxyGroupLastMemberFallsBackToAuto(t *testing.T) {
	tun := startPersonalProxiesTestTunnel(t)
	tun.inviterTickets = &fakeInviterTickets{byFriend: map[string][]string{}}
	require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer(friendA)}}))
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))

	require.NoError(t, tun.updateOutbounds(servers.ServerList{Servers: []*servers.Server{directServer("lantern-1")}}))
	assert.Nil(t, tun.personalProxyGroup())
	assert.Nil(t, tun.personalProxiesGroup())
	assert.Equal(t, AutoSelectTag, tun.clashServer.selectedMode())
}

// A ticket-key rotation changes every tag: the selected friend's tag leaves
// the config and the tickets at once, and the group goes with it.
func TestPersonalProxyGroupGoesWhenItsTagIsRotated(t *testing.T) {
	const rotated = "helper-abababababab"
	tun, tickets := startPersonalProxyTestTunnel(t, friendA)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))

	tickets.set(friendA)
	tickets.set(rotated, "ticket-"+rotated)
	require.NoError(t, tun.reconcileHelpers(servers.ServerList{Servers: []*servers.Server{
		consumerServer(consumer), directServer(rotated),
	}}))
	assert.Nil(t, tun.personalProxyGroup())
	assert.Equal(t, PersonalProxiesTag, selectedInManual(t, tun))
	assert.Equal(t, []string{"ticket-" + rotated}, tun.roomTickets())
}

// Until the ticket source is answered the consumer may still reach the friend,
// so it stays in the group.
func TestPersonalProxyGroupKeepsTheConsumerUntilTicketsAreKnown(t *testing.T) {
	tun, tickets := startPersonalProxyTestTunnel(t, friendA)
	tickets.set(friendA)
	tickets.unanswered = true
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	assert.ElementsMatch(t, []string{friendA, consumer}, personalProxyMembers(tun))
	awaitFirstProbe(t, tun, friendA)

	tickets.unanswered = false
	require.NoError(t, tun.reconcileHelpers(servers.ServerList{Servers: []*servers.Server{
		consumerServer(consumer), directServer(friendA),
	}}))
	assert.Equal(t, []string{friendA}, personalProxyMembers(tun), "answered without a ticket: direct only")
}

func TestPersonalProxyMemberReportsTheDirectOutbound(t *testing.T) {
	tun, _ := startPersonalProxyTestTunnel(t, friendA)
	assert.Equal(t, PersonalProxyMember{Direct: true}, tun.personalProxyMember(friendA))
	assert.Equal(t, PersonalProxyMember{}, tun.personalProxyMember(consumer))
	assert.Equal(t, PersonalProxyMember{}, tun.personalProxyMember(friendB))
	tun.clashServer.HistoryStorage().StoreURLTestHistory(friendA, &adapter.URLTestHistory{Delay: 42})
	assert.Equal(t, PersonalProxyMember{Direct: true, DirectDelay: 42}, tun.personalProxyMember(friendA))
}

func TestBuildOptions_PersonalProxy(t *testing.T) {
	standInRoomsConsumer(t)
	withHelpers := func(t *testing.T, tags ...string) O.Options {
		cfg := testConfig(t)
		for _, tag := range tags {
			if tag == consumer {
				cfg.Options.Outbounds = append(cfg.Options.Outbounds, consumerServer(consumer).Options.(O.Outbound))
				continue
			}
			cfg.Options.Outbounds = append(cfg.Options.Outbounds, helperOutbound(tag))
		}
		return cfg.Options
	}
	members := func(opts O.Options) []string {
		group := findOutbound(opts.Outbounds, PersonalProxyTag)
		if group == nil {
			return nil
		}
		assert.Equal(t, lbC.TypeMutableURLTest, group.Type)
		return group.Options.(*lbO.MutableURLTestOutboundOptions).Outbounds
	}
	build := func(t *testing.T, options O.Options, rooms bool) (O.Options, *lbO.MutableSelectorOutboundOptions) {
		opts, err := buildOptions(BoxOptions{BasePath: t.TempDir(), Options: options,
			InitialServer: PersonalProxySelection(friendA), personalProxyRooms: rooms})
		require.NoError(t, err)
		return opts, findOutbound(opts.Outbounds, ManualSelectTag).Options.(*lbO.MutableSelectorOutboundOptions)
	}

	t.Run("starts selected over the friend's members", func(t *testing.T) {
		opts, manual := build(t, withHelpers(t, friendA, friendB, consumer), true)
		assert.Equal(t, []string{friendA, consumer}, members(opts))
		assert.Equal(t, PersonalProxyTag, manual.Outbounds[0])
		assert.Equal(t, ManualSelectTag, opts.Experimental.ClashAPI.DefaultMode)

		opts, _ = build(t, withHelpers(t, friendA, friendB, consumer), false)
		assert.Equal(t, []string{friendA}, members(opts), "no rooms ticket, no consumer")
	})

	t.Run("falls back to the personal proxies group without members", func(t *testing.T) {
		opts, manual := build(t, withHelpers(t, friendB, consumer), false)
		assert.Nil(t, members(opts))
		assert.NotContains(t, manual.Outbounds, PersonalProxyTag)
		assert.Equal(t, PersonalProxiesTag, manual.Outbounds[0])
	})

	t.Run("falls back to auto-select without helpers", func(t *testing.T) {
		options, _ := testBoxOptions(t)
		opts, _ := build(t, options, true)
		assert.Nil(t, members(opts))
		assert.Equal(t, AutoSelectTag, opts.Experimental.ClashAPI.DefaultMode)
	})
}

// A tunnel built with a friend selected narrows from its first subscribe;
// one built without a group for it does not.
func TestNewTunnelNarrowsToTheInitialFriend(t *testing.T) {
	c := NewVPNClient(t.TempDir(), nil, nil)
	tickets := &fakeInviterTickets{byFriend: map[string][]string{friendA: {"a"}, friendB: {"b"}}}
	c.SetInviterTickets(tickets)
	tn := c.newTunnel(BoxOptions{InitialServer: PersonalProxySelection(friendA)})
	assert.Equal(t, []string{"a"}, tn.roomTickets())
	assert.True(t, c.withPersonalProxyRooms(BoxOptions{InitialServer: PersonalProxySelection(friendA)}).personalProxyRooms)
	assert.False(t, c.withPersonalProxyRooms(BoxOptions{InitialServer: PersonalProxySelection(friendC)}).personalProxyRooms)
	assert.Equal(t, []string{"a", "b"}, c.newTunnel(BoxOptions{InitialServer: friendA}).roomTickets())
}

// The group the tunnel creates at runtime carries traffic.
func TestPersonalProxyGroupDials(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(target.Close)
	tun, tickets := startPersonalProxyTestTunnel(t, friendA)
	tickets.set(friendA)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	group, ok := tun.personalProxyGroup().(adapter.Outbound)
	require.True(t, ok)
	conn, err := group.DialContext(t.Context(), "tcp", M.ParseSocksaddr(target.Listener.Addr().String()))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// A rooms consumer with no connection (rooms-only, before its friend's
// ticket or room arrives) fails its dials; the group then carries traffic
// over the friend's direct outbound rather than failing.
func TestPersonalProxyGroupFailsOverFromADeadConsumer(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(target.Close)
	tun, _ := startPersonalProxyTestTunnel(t, friendA)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	require.ElementsMatch(t, []string{friendA, consumer}, personalProxyMembers(tun))
	group, ok := tun.personalProxyGroup().(adapter.Outbound)
	require.True(t, ok)
	for range 5 {
		conn, err := group.DialContext(t.Context(), "tcp", M.ParseSocksaddr(target.Listener.Addr().String()))
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	}
}
