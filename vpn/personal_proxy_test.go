package vpn

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling"
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

// A rooms consumer whose dials fail at once leaves the group carrying
// traffic over the friend's direct outbound.
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

// A real helper consumer with no connected slot must not hold a dial that
// the friend's direct outbound could carry. The consumer is lantern-box's own
// Unbounded outbound, against a discovery server that never offers a peer,
// because that outbound failing at once is the guarantee under test. Every
// probe fails, so no member has URL-test history and each group's first pick
// is arbitrary; each fresh tunnel picks again.
func TestPersonalProxyGroupDoesNotWaitOnAnUnconnectedConsumer(t *testing.T) {
	// The consumer signals through kindling, which caches into the data
	// directory once it first sends.
	dataDir := t.TempDir()
	settings.Reset()
	require.NoError(t, settings.InitSettings(dataDir))
	require.NoError(t, settings.Set(settings.DataPathKey, dataDir))
	require.NoError(t, kindling.Close())
	t.Cleanup(func() {
		_ = kindling.Close()
		settings.Reset()
	})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(target.Close)
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(discovery.Close)
	for range 4 {
		tun := startPersonalProxiesTestTunnel(t)
		setPersonalProxiesProbe(t, "http://127.0.0.1:1/", personalProxiesProbeInterval)
		tun.inviterTickets = &fakeInviterTickets{byFriend: map[string][]string{friendA: {"ticket-" + friendA}}}
		require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{
			{Tag: consumer, Type: lbC.TypeUnbounded, IsLantern: true,
				Options: O.Outbound{Tag: consumer, Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{
					DiscoverySrv: discovery.URL, InsecureDoNotVerifyClientCert: true,
				}}},
			directServer(friendA),
		}}))
		require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
		require.ElementsMatch(t, []string{friendA, consumer}, personalProxyMembers(tun))
		for name, group := range map[string]lbA.MutableOutboundGroup{
			PersonalProxyTag: tun.personalProxyGroup(), PersonalProxiesTag: tun.personalProxiesGroup(),
		} {
			out, ok := group.(adapter.Outbound)
			require.True(t, ok, name)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			start := time.Now()
			conn, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(target.Listener.Addr().String()))
			cancel()
			require.NoError(t, err, name)
			require.NoError(t, conn.Close())
			require.Less(t, time.Since(start), 2*time.Second, "%s waited on the consumer", name)
		}
	}
}

// startRoutedPersonalProxyTestTunnel starts a test tunnel with a mixed
// inbound, and returns it with a dial that opens an echoed connection through
// that inbound.
func startRoutedPersonalProxyTestTunnel(t *testing.T) (*tunnel, func() net.Conn) {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	inbound, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := uint16(inbound.Addr().(*net.TCPAddr).Port)
	require.NoError(t, inbound.Close())
	listen := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	tun := startPersonalProxiesTestTunnelWith(t, []O.Inbound{{Type: C.TypeMixed, Tag: "mixed-in",
		Options: &O.HTTPMixedInboundOptions{ListenOptions: O.ListenOptions{Listen: &listen, ListenPort: port}}}})
	client := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", port), socks.Version5, "", "")
	dial := func() net.Conn {
		t.Helper()
		conn, err := client.DialContext(t.Context(), N.NetworkTCP, M.ParseSocksaddr(echo.Addr().String()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		requireEcho(t, conn)
		return conn
	}
	return tun, dial
}

func requireEcho(t *testing.T, conn net.Conn) {
	t.Helper()
	_, err := conn.Write([]byte("ping"))
	require.NoError(t, err)
	_, err = io.ReadFull(conn, make([]byte, 4))
	require.NoError(t, err)
}

// requireClosed fails the test unless a read on conn ends in a non-timeout
// error within two seconds.
func requireClosed(t *testing.T, conn net.Conn, msg string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	var err error
	for err == nil {
		_, err = conn.Read(make([]byte, 64))
	}
	var netErr net.Error
	assert.False(t, errors.As(err, &netErr) && netErr.Timeout(), msg)
}

// awaitRoutedLeaf waits for a connection through the personal proxy group
// recorded under leaf.
func awaitRoutedLeaf(t *testing.T, tun *tunnel, leaf string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, c := range tun.clashServer.connTracker.activeConnections() {
			if slices.Contains(c.ChainList, PersonalProxyTag) && strings.HasSuffix(c.Outbound, "/"+leaf) {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "a connection through the group recorded under %s", leaf)
}

// Switching friends keeps the manual selector on the same group tag, so the
// selector interrupts nothing; connections through the previous friend's
// group must still be closed, while reselecting the same friend keeps its
// connections.
func TestPersonalProxySwitchClosesThePreviousFriendsConnections(t *testing.T) {
	standInRoomsConsumer(t)
	tun, dial := startRoutedPersonalProxyTestTunnel(t)
	// No rooms tickets: each friend's group holds only its direct outbound.
	tun.inviterTickets = &fakeInviterTickets{byFriend: map[string][]string{}}
	require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer(friendA), directServer(friendB)}}))
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	viaA := dial()
	awaitRoutedLeaf(t, tun, friendA)

	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	requireClosed(t, viaA, "the connection through the previous friend was left open")

	viaB := dial()
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	requireEcho(t, viaB)
}

// A switch that narrows to the new friend but then fails leaves the previous
// friend's group installed; retrying the same friend must still close the
// connections through it.
func TestPersonalProxyRetriedSwitchClosesThePreviousFriendsConnections(t *testing.T) {
	standInRoomsConsumer(t)
	tun, dial := startRoutedPersonalProxyTestTunnel(t)
	tun.inviterTickets = &fakeInviterTickets{byFriend: map[string][]string{}}
	require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{
		consumerServer(consumer), directServer(friendA), directServer(friendB),
	}}))
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	viaA := dial()
	awaitRoutedLeaf(t, tun, friendA)

	// selectMode fails without a box, after the consumers have restarted.
	box := tun.boxInstance
	tun.boxInstance = nil
	consumerOut := tun.liveOutbound(consumer)
	require.Error(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	tun.boxInstance = box
	require.NotSame(t, consumerOut, tun.liveOutbound(consumer), "the failure came after the restart")
	require.Equal(t, friendB, tun.selectedFriend())

	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	requireClosed(t, viaA, "the retry left the previous friend's connection open")
}

// connectProxy serves HTTP CONNECT, standing in for a rooms consumer that
// carries connections.
func connectProxy(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				if _, err := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
					return
				}
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

// A switch closes connections recorded under the rooms consumer even though
// the next friend's group also holds it: such a record may hide a connection
// the previous friend's direct outbound carried after the consumer failed.
func TestPersonalProxySwitchClosesConnectionsRecordedUnderTheConsumer(t *testing.T) {
	prev := roomsConsumerType
	roomsConsumerType = C.TypeHTTP
	t.Cleanup(func() { roomsConsumerType = prev })
	tun, dial := startRoutedPersonalProxyTestTunnel(t)
	tun.inviterTickets = &fakeInviterTickets{byFriend: map[string][]string{
		friendA: {"ticket-" + friendA}, friendB: {"ticket-" + friendB},
	}}
	require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{
		{Tag: consumer, Type: C.TypeHTTP, Options: O.Outbound{Tag: consumer, Type: C.TypeHTTP,
			Options: &O.HTTPOutboundOptions{ServerOptions: O.ServerOptions{Server: "127.0.0.1", ServerPort: connectProxy(t)}}}},
		directServer(friendB),
	}}))
	// Friend A is rooms-only, so its group's only member is the consumer.
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	awaitFirstProbe(t, tun, consumer)
	require.Eventually(t, func() bool { return tun.personalProxyGroup().(adapter.OutboundGroup).Now() == consumer },
		5*time.Second, 10*time.Millisecond)
	viaA := dial()
	awaitRoutedLeaf(t, tun, consumer)

	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	require.Contains(t, personalProxyMembers(tun), consumer, "the next group holds the consumer too")
	requireClosed(t, viaA, "a connection recorded under the consumer was left open")
}

// A rooms consumer that could not be restarted under the new friend's
// narrowing would carry its slots to the previous friend into the new group,
// so the selection is refused and the previous one kept as it was; once the
// cause clears, the same selection restarts the consumer and succeeds.
func TestPersonalProxySelectionRefusedWhenAConsumerRestartFails(t *testing.T) {
	tun, tickets := startPersonalProxyTestTunnel(t, friendA, friendB)
	tickets.set(friendC, "ticket-"+friendC)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	good, ok := tun.optsMap.Load(consumer)
	require.True(t, ok)
	tun.optsMap.Store(consumer, []byte("{"))
	consumerOut, groupOut := tun.liveOutbound(consumer), tun.liveOutbound(PersonalProxyTag)

	for _, friend := range []string{friendB, friendC} {
		require.Error(t, tun.selectOutbound(PersonalProxySelection(friend)), friend)
		assert.Equal(t, friendA, tun.selectedFriend(), "the previous narrowing is restored")
		assert.Equal(t, []string{"ticket-" + friendA}, tun.roomTickets())
		assert.Same(t, groupOut, tun.liveOutbound(PersonalProxyTag), "no group for the new friend")
		assert.ElementsMatch(t, []string{friendA, consumer}, personalProxyMembers(tun))
		assert.Equal(t, PersonalProxyTag, selectedInManual(t, tun))
		assert.Same(t, consumerOut, tun.liveOutbound(consumer))
	}

	tun.optsMap.Store(consumer, good)
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	assert.NotSame(t, consumerOut, tun.liveOutbound(consumer), "the retry restarts the consumer")
	assert.ElementsMatch(t, []string{friendB, consumer}, personalProxyMembers(tun))
	assert.Equal(t, []string{"ticket-" + friendB}, tun.roomTickets())
}
