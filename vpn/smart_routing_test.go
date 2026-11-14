package vpn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/servers"
)

const testSmartRoutingTag = "sr-ai"

// testOtherGroupTag is a non-selectable group that isn't a smart-routing one.
const testOtherGroupTag = "infra"

// startSmartRoutingTestTunnel starts a tunnel holding the direct outbound and
// a server for each of tags, with the smart-routing group testSmartRoutingTag
// over members and the group testOtherGroupTag over the direct outbound and
// the first of tags.
func startSmartRoutingTestTunnel(t *testing.T, tags, members []string) *tunnel {
	t.Helper()
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	outbounds := []O.Outbound{{Type: C.TypeDirect, Tag: C.TypeDirect, Options: &O.DirectOutboundOptions{}}}
	for _, tag := range tags {
		outbounds = append(outbounds, O.Outbound{Type: C.TypeDirect, Tag: tag, Options: &O.DirectOutboundOptions{}})
	}
	outbounds = append(outbounds,
		O.Outbound{Type: lbC.TypeMutableURLTest, Tag: testSmartRoutingTag,
			Options: &lbO.MutableURLTestOutboundOptions{Outbounds: members, URL: probe.URL}},
		O.Outbound{Type: lbC.TypeMutableURLTest, Tag: testOtherGroupTag,
			Options: &lbO.MutableURLTestOutboundOptions{Outbounds: []string{C.TypeDirect, tags[0]}, URL: probe.URL}},
		O.Outbound{Type: lbC.TypeMutableSelector, Tag: ManualSelectTag,
			Options: &lbO.MutableSelectorOutboundOptions{Outbounds: tags}},
		O.Outbound{Type: lbC.TypeMutableAutoSelect, Tag: AutoSelectTag,
			Options: &lbO.MutableAutoSelectOutboundOptions{Outbounds: tags, URL: probe.URL}},
	)
	return startTestTunnel(t, outbounds, nil)
}

func serverList(smartRouting map[string][]string, tags ...string) servers.ServerList {
	list := servers.ServerList{SmartRouting: smartRouting}
	for _, tag := range tags {
		list.Servers = append(list.Servers, directServer(tag))
	}
	return list
}

func TestUpdateOutboundsAddsSmartRoutingMembers(t *testing.T) {
	tun := startSmartRoutingTestTunnel(t, []string{"lantern-1", "lantern-2"}, []string{"lantern-1", "lantern-2"})

	require.NoError(t, tun.updateOutbounds(serverList(
		map[string][]string{testSmartRoutingTag: {"lantern-2", "lantern-3"}},
		"lantern-1", "lantern-2", "lantern-3",
	)))
	assert.ElementsMatch(t, []string{"lantern-1", "lantern-2", "lantern-3"}, selectorMembers(t, tun, testSmartRoutingTag),
		"a new member joins and a retained one the config doesn't list stays")

	require.NoError(t, tun.updateOutbounds(serverList(
		map[string][]string{testSmartRoutingTag: {"lantern-3", "lantern-4"}},
		"lantern-1", "lantern-3", "lantern-4",
	)))
	assert.ElementsMatch(t, []string{"lantern-1", "lantern-3", "lantern-4"}, selectorMembers(t, tun, testSmartRoutingTag),
		"a server removed from the tunnel leaves the group")

	require.NoError(t, tun.updateOutbounds(serverList(nil, "lantern-1", "lantern-3", "lantern-4")))
	assert.ElementsMatch(t, []string{"lantern-1", "lantern-3", "lantern-4"}, selectorMembers(t, tun, testSmartRoutingTag),
		"a list without smart routing leaves the members alone")

	require.NoError(t, tun.updateOutbounds(serverList(
		map[string][]string{testSmartRoutingTag: {"unknown"}},
		"lantern-1", "lantern-3", "lantern-4",
	)))
	assert.ElementsMatch(t, []string{"lantern-1", "lantern-3", "lantern-4"}, selectorMembers(t, tun, testSmartRoutingTag),
		"a member the tunnel doesn't hold doesn't join")

	require.NoError(t, tun.updateOutbounds(serverList(
		map[string][]string{testSmartRoutingTag: {"lantern-5", "lantern-6"}},
		"lantern-1", "lantern-5", "lantern-6",
	)))
	assert.ElementsMatch(t, []string{"lantern-1", "lantern-5", "lantern-6"}, selectorMembers(t, tun, testSmartRoutingTag),
		"new members join as the old ones leave")
	assert.ElementsMatch(t, []string{C.TypeDirect, "lantern-1"}, selectorMembers(t, tun, testOtherGroupTag),
		"a group the smart-routing rules don't name keeps its members")
}

func TestUpdateOutboundsKeepsReservedSmartRoutingMembers(t *testing.T) {
	members := []string{C.TypeDirect, "lantern-1"}
	tun := startSmartRoutingTestTunnel(t, []string{"lantern-1", "lantern-2"}, members)

	require.NoError(t, tun.updateOutbounds(serverList(
		map[string][]string{testSmartRoutingTag: members},
		"lantern-1", "lantern-2",
	)))
	assert.ElementsMatch(t, members, selectorMembers(t, tun, testSmartRoutingTag),
		"direct isn't an auto member but stays in the group")
}

func TestUpdateOutboundsSkipsRemovedSmartRoutingMembers(t *testing.T) {
	tun := startSmartRoutingTestTunnel(t, []string{"lantern-1", "lantern-2"}, []string{"lantern-2"})

	// What removeOutboundsLocked leaves while lantern-1's teardown is queued.
	for _, tag := range []string{ManualSelectTag, AutoSelectTag} {
		group, ok := tun.mutGrpMgr.OutboundGroup(tag)
		require.True(t, ok, tag)
		_, err := group.Remove("lantern-1")
		require.NoError(t, err)
	}
	tun.optsMap.Delete("lantern-1")
	require.NotNil(t, tun.liveOutbound("lantern-1"))

	require.NoError(t, tun.updateOutbounds(serverList(
		map[string][]string{testSmartRoutingTag: {"lantern-1", "lantern-2"}},
		"lantern-2",
	)))
	assert.Equal(t, []string{"lantern-2"}, selectorMembers(t, tun, testSmartRoutingTag),
		"a server removed from the tunnel doesn't rejoin while its teardown is queued")
}

func TestRemoveOutboundsLeavesSmartRoutingGroups(t *testing.T) {
	tun := startSmartRoutingTestTunnel(t, []string{"lantern-1", "lantern-2"}, []string{"lantern-1", "lantern-2"})

	require.NoError(t, tun.removeOutbounds([]string{"lantern-1"}))
	assert.Equal(t, []string{"lantern-2"}, selectorMembers(t, tun, testSmartRoutingTag))
	assert.NotContains(t, selectorMembers(t, tun, AutoSelectTag), "lantern-1")
	assert.Equal(t, []string{C.TypeDirect}, selectorMembers(t, tun, testOtherGroupTag),
		"a server removed from the tunnel leaves every group")
	requireTornDown(t, tun, "lantern-1")
}
