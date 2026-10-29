package vpn

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/servers"
)

const (
	testHelper1 = "helper-0123456789ab"
	testHelper2 = "helper-ba9876543210"
)

func directServer(tag string) *servers.Server {
	return &servers.Server{Tag: tag, Type: C.TypeDirect, IsLantern: true,
		Options: O.Outbound{Tag: tag, Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}}}
}

// startPersonalProxiesTestTunnel starts a tunnel with lantern-1 and helpers,
// built as buildOptions builds it: the group exists only when helpers do.
func startPersonalProxiesTestTunnel(t *testing.T, helpers ...string) *tunnel {
	t.Helper()
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	setPersonalProxiesProbe(t, probe.URL, personalProxiesProbeInterval)
	members := append([]string{"lantern-1"}, helpers...)
	outbounds := []O.Outbound{{Type: C.TypeDirect, Tag: "lantern-1", Options: &O.DirectOutboundOptions{}}}
	for _, h := range helpers {
		outbounds = append(outbounds, O.Outbound{Type: C.TypeDirect, Tag: h, Options: &O.DirectOutboundOptions{}})
	}
	selectable := slices.Clone(members)
	if len(helpers) > 0 {
		outbounds = append(outbounds, O.Outbound{Type: lbC.TypeMutableURLTest, Tag: PersonalProxiesTag,
			Options: &lbO.MutableURLTestOutboundOptions{Outbounds: helpers, URL: probe.URL}})
		selectable = append(selectable, PersonalProxiesTag)
	}
	outbounds = append(outbounds,
		O.Outbound{Type: lbC.TypeMutableSelector, Tag: ManualSelectTag,
			Options: &lbO.MutableSelectorOutboundOptions{Outbounds: selectable}},
		O.Outbound{Type: lbC.TypeMutableAutoSelect, Tag: AutoSelectTag,
			Options: &lbO.MutableAutoSelectOutboundOptions{Outbounds: members, URL: probe.URL}},
	)
	options := O.Options{
		Log:       &O.LogOptions{Disabled: true},
		Outbounds: outbounds,
		Route:     &O.RouteOptions{Rules: []O.Rule{selectModeRule(AutoSelectTag), selectModeRule(ManualSelectTag)}},
		Experimental: &O.ExperimentalOptions{
			ClashAPI:  &O.ClashAPIOptions{ModeList: []string{ManualSelectTag, AutoSelectTag}, DefaultMode: AutoSelectTag},
			CacheFile: &O.CacheFileOptions{Enabled: true, Path: filepath.Join(t.TempDir(), "cache.db")},
		},
	}
	tun := &tunnel{dataPath: t.TempDir()}
	t.Cleanup(func() { assert.NoError(t, tun.close()) })
	require.NoError(t, tun.init(t.Context(), options, nil))
	require.NoError(t, tun.boxInstance.Start())
	tun.optsMap = makeOutboundOptsMap(tun.ctx, options)
	tun.nonSelectableTags = makeNonSelectableTagSet(nil, options)
	tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
	clash := service.FromContext[adapter.ClashServer](tun.ctx).(*clashServer)
	tun.clashServer = clash
	mutGrpMgr, err := newMutableGroupManager(tun.ctx, tun.logFactory.NewLogger("groupsManager"), clash.connTracker)
	require.NoError(t, err)
	tun.mutGrpMgr = mutGrpMgr
	t.Cleanup(mutGrpMgr.Close)
	return tun
}

// setPersonalProxiesProbe points the URL tests of groups the tunnel creates
// at url, every interval, for the rest of the test.
func setPersonalProxiesProbe(t *testing.T, url string, interval time.Duration) {
	t.Helper()
	prevURL, prevInterval := personalProxiesProbeURL, personalProxiesProbeInterval
	personalProxiesProbeURL, personalProxiesProbeInterval = url, interval
	t.Cleanup(func() {
		personalProxiesProbeURL, personalProxiesProbeInterval = prevURL, prevInterval
	})
}

func selectorMembers(t *testing.T, tun *tunnel, tag string) []string {
	t.Helper()
	g, ok := tun.mutGrpMgr.OutboundGroup(tag)
	require.True(t, ok, tag)
	return slices.Clone(g.All())
}

// personalProxiesMembers returns the group's members, or nil when the group
// does not exist.
func personalProxiesMembers(tun *tunnel) []string {
	g := tun.personalProxiesGroup()
	if g == nil {
		return nil
	}
	return slices.Clone(g.All())
}

func TestPersonalProxiesGroupFollowsHelperOutbounds(t *testing.T) {
	tun := startPersonalProxiesTestTunnel(t)
	assert.Nil(t, tun.personalProxiesGroup(), "no helpers, no group")
	assert.NotContains(t, selectorMembers(t, tun, ManualSelectTag), PersonalProxiesTag)
	require.ErrorIs(t, tun.selectOutbound(PersonalProxiesTag), ErrPersonalProxiesUnavailable)

	require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{
		directServer(testHelper1), directServer(testHelper2), directServer("lantern-2"),
	}}))
	assert.ElementsMatch(t, []string{testHelper1, testHelper2}, personalProxiesMembers(tun))
	for _, tag := range personalProxiesMembers(tun) {
		assert.True(t, lbA.IsHelperTag(tag), "only helper outbounds: %s", tag)
	}
	assert.Contains(t, selectorMembers(t, tun, ManualSelectTag), PersonalProxiesTag)
	assert.NotContains(t, selectorMembers(t, tun, AutoSelectTag), PersonalProxiesTag)
	assert.Contains(t, selectorMembers(t, tun, AutoSelectTag), testHelper1, "Smart Location keeps the helpers")

	require.NoError(t, tun.selectOutbound(PersonalProxiesTag))
	sel, _ := tun.mutGrpMgr.OutboundGroup(ManualSelectTag)
	assert.Equal(t, PersonalProxiesTag, sel.Now())

	// A config without testHelper1 keeps the group and the selection.
	require.NoError(t, tun.updateOutbounds(servers.ServerList{Servers: []*servers.Server{
		directServer("lantern-1"), directServer(testHelper2),
	}}))
	assert.Equal(t, []string{testHelper2}, personalProxiesMembers(tun))
	assert.Equal(t, PersonalProxiesTag, sel.Now(), "an update must not withdraw the selected group")

	// The last helper goes: so do the group and its selector entry.
	require.NoError(t, tun.updateOutbounds(servers.ServerList{Servers: []*servers.Server{directServer("lantern-1")}}))
	assert.Nil(t, tun.personalProxiesGroup())
	assert.NotContains(t, selectorMembers(t, tun, ManualSelectTag), PersonalProxiesTag)
	require.ErrorIs(t, tun.selectOutbound(PersonalProxiesTag), ErrPersonalProxiesUnavailable)

	// And it comes back, never having started empty.
	require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer(testHelper1)}}))
	assert.Equal(t, []string{testHelper1}, personalProxiesMembers(tun))
	assert.Contains(t, selectorMembers(t, tun, ManualSelectTag), PersonalProxiesTag)
}

func TestPersonalProxiesGroupBuiltAtStartIsManagedLikeARuntimeOne(t *testing.T) {
	tun := startPersonalProxiesTestTunnel(t, testHelper1)
	_, registered := tun.mutGrpMgr.OutboundGroup(PersonalProxiesTag)
	assert.False(t, registered, "the group is the tunnel's, not the group manager's")
	require.NoError(t, tun.selectOutbound(PersonalProxiesTag))

	require.NoError(t, tun.removeOutbounds([]string{testHelper1}))
	assert.Nil(t, tun.personalProxiesGroup())
	assert.NotContains(t, selectorMembers(t, tun, ManualSelectTag), PersonalProxiesTag)
}

// A url-test group started empty never gets a pause manager, and its first
// dial after an Add panics in its check loop. Dialing through a group the
// tunnel recreated at runtime starts that loop; a URL test from one of its
// ticks shows the loop got past registering with the pause manager.
func TestPersonalProxiesGroupRecreatedAtRuntimeDials(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(target.Close)
	tun := startPersonalProxiesTestTunnel(t)
	setPersonalProxiesProbe(t, personalProxiesProbeURL, 50*time.Millisecond)
	history := service.FromContext[adapter.URLTestHistoryStorage](tun.ctx)
	if history == nil {
		history = tun.clashServer.HistoryStorage()
	}
	probed := func() bool { return history.LoadURLTestHistory(testHelper1) != nil }
	for range 2 {
		require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer(testHelper1)}}))
		group, ok := tun.personalProxiesGroup().(adapter.Outbound)
		require.True(t, ok)
		require.Eventually(t, probed, 5*time.Second, 10*time.Millisecond, "the group probes on start")
		conn, err := group.DialContext(t.Context(), "tcp", M.ParseSocksaddr(target.Listener.Addr().String()))
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		// The loop may probe once before registering, so wait out two probes.
		for range 2 {
			history.DeleteURLTestHistory(testHelper1)
			require.Eventually(t, probed, 5*time.Second, 10*time.Millisecond, "the check loop ticks")
		}

		require.NoError(t, tun.removeOutbounds([]string{testHelper1}))
		require.Nil(t, tun.personalProxiesGroup())
		history.DeleteURLTestHistory(testHelper1)
	}
}

func blockServer(tag string) *servers.Server {
	return &servers.Server{Tag: tag, Type: C.TypeBlock, IsLantern: true,
		Options: O.Outbound{Tag: tag, Type: C.TypeBlock, Options: &O.StubOptions{}}}
}

// A helper whose options change is replaced in the outbound manager under the
// same tag; the group must dial through the replacement, not the closed
// original, whether or not another member stays in the group meanwhile.
func TestPersonalProxiesGroupFollowsAReplacedHelper(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(target.Close)
	dest := M.ParseSocksaddr(target.Listener.Addr().String())
	for _, tc := range []struct {
		name   string
		others []*servers.Server
	}{
		{"only member", nil},
		{"alongside another member", []*servers.Server{blockServer(testHelper2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tun := startPersonalProxiesTestTunnel(t)
			require.NoError(t, tun.addOutbounds(servers.ServerList{
				Servers: append([]*servers.Server{blockServer(testHelper1)}, tc.others...)}))
			group := tun.personalProxiesGroup().(adapter.Outbound)
			_, err := group.DialContext(t.Context(), "tcp", dest)
			require.Error(t, err, "every member blocks")

			require.NoError(t, tun.selectOutbound(PersonalProxiesTag))

			// addOutbounds also reports that the manual selector kept its own
			// member (a pre-existing limit of replacing an outbound in place).
			_ = tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer(testHelper1)}})
			group = tun.personalProxiesGroup().(adapter.Outbound)
			sel, _ := tun.mutGrpMgr.OutboundGroup(ManualSelectTag)
			assert.Equal(t, PersonalProxiesTag, sel.Now(), "the selection stays on the group")
			assert.ElementsMatch(t, append([]string{testHelper1}, tagsOf(tc.others)...), personalProxiesMembers(tun))
			require.Eventually(t, func() bool {
				conn, err := group.DialContext(t.Context(), "tcp", dest)
				if err != nil {
					return false
				}
				_ = conn.Close()
				return true
			}, 5*time.Second, 50*time.Millisecond, "the group dials through the replaced helper")
		})
	}
}

func tagsOf(list []*servers.Server) []string {
	var tags []string
	for _, s := range list {
		tags = append(tags, s.Tag)
	}
	return tags
}

// A rebuild of the selected group that fails after withdrawing it leaves the
// selector on another tag; the sync that recreates the group selects it
// again, unless the user has chosen something else meanwhile.
func TestPersonalProxiesSelectionSurvivesAFailedRebuild(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reselect string
		want     string
	}{
		{"selected again", "", PersonalProxiesTag},
		{"a later choice wins", "lantern-1", "lantern-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tun := startPersonalProxiesTestTunnel(t)
			require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{blockServer(testHelper1)}}))
			require.NoError(t, tun.selectOutbound(PersonalProxiesTag))
			sel, _ := tun.mutGrpMgr.OutboundGroup(ManualSelectTag)

			// An interval beyond the idle timeout makes creating the group fail.
			interval := personalProxiesProbeInterval
			personalProxiesProbeInterval = urlTestIdleTimeout + time.Minute
			err := tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer(testHelper1)}})
			personalProxiesProbeInterval = interval
			require.ErrorContains(t, err, "create personal proxies group")
			require.Nil(t, tun.personalProxiesGroup())
			require.NotEqual(t, PersonalProxiesTag, sel.Now())

			if tc.reselect != "" {
				require.NoError(t, tun.selectOutbound(tc.reselect))
			}
			require.NoError(t, tun.addOutbounds(servers.ServerList{Servers: []*servers.Server{directServer("lantern-2")}}))
			require.NotNil(t, tun.personalProxiesGroup())
			assert.Equal(t, tc.want, sel.Now())
		})
	}
}
