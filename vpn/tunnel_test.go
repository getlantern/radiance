package vpn

import (
	"path/filepath"
	"testing"
	"time"

	sbA "github.com/sagernet/sing-box/adapter"
	sbC "github.com/sagernet/sing-box/constant"
	sbO "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn/ipc"
)

func TestEstablishConnection(t *testing.T) {
	common.SetPathsForTesting(t)
	tOpts, _, err := testBoxOptions(settings.GetString(settings.DataPathKey))
	require.NoError(t, err, "failed to get test box options")

	testEstablishConnection(t, *tOpts)
	tun := tInstance
	assert.NoError(t, tun.Close(), "failed to close lbService")
	assert.Equal(t, ipc.StatusClosed, tun.Status(), "tun should be closed")
}

func TestUpdateServers(t *testing.T) {
	common.SetPathsForTesting(t)
	testOpts, _, err := testBoxOptions(settings.GetString(settings.DataPathKey))
	require.NoError(t, err, "failed to get test box options")

	baseOuts := baseOpts(settings.GetString(settings.DataPathKey)).Outbounds
	allOutbounds := map[string]sbO.Outbound{
		"direct": baseOuts[0],
		"block":  baseOuts[1],
	}
	for _, out := range testOpts.Outbounds {
		switch out.Type {
		case sbC.TypeHTTP, sbC.TypeSOCKS:
			allOutbounds[out.Tag] = out
		default:
		}
	}

	lanternTags := []string{"http1-out", "http2-out", "socks1-out"}
	userTags := []string{}
	outs := []sbO.Outbound{
		allOutbounds["direct"], allOutbounds["block"],
		allOutbounds["http1-out"], allOutbounds["http2-out"], allOutbounds["socks1-out"],
		urlTestOutbound(autoLanternTag, lanternTags), urlTestOutbound(autoUserTag, userTags),
		selectorOutbound(servers.SGLantern, append(lanternTags, autoLanternTag)),
		selectorOutbound(servers.SGUser, append(userTags, autoUserTag)),
		urlTestOutbound(autoAllTag, []string{autoLanternTag, autoUserTag}),
	}

	testOpts.Outbounds = outs
	testEstablishConnection(t, *testOpts)
	tun := tInstance
	defer func() {
		tun.Close()
	}()

	time.Sleep(500 * time.Millisecond)

	newOpts := servers.Options{
		Outbounds: []sbO.Outbound{
			allOutbounds["http1-out"], allOutbounds["socks2-out"],
		},
	}
	err = tInstance.updateGroup(servers.SGLantern, newOpts)
	require.NoError(t, err, "failed to update servers for lantern")

	time.Sleep(250 * time.Millisecond)

	outboundMgr := service.FromContext[sbA.OutboundManager](tun.ctx)
	require.NotNil(t, outboundMgr, "outbound manager should not be nil")

	groups := tun.mutGrpMgr.OutboundGroups()

	want := map[string][]string{
		autoAllTag:        {autoLanternTag, autoUserTag},
		servers.SGLantern: {autoLanternTag, "http1-out", "socks2-out"},
		autoLanternTag:    {"http1-out", "socks2-out"},
		servers.SGUser:    {autoUserTag},
		autoUserTag:       {},
	}
	got := make(map[string][]string)
	allTags := []string{"direct", "block", autoAllTag, autoLanternTag, autoUserTag, servers.SGLantern, servers.SGUser}
	for _, g := range groups {
		tags := g.All()
		got[g.Tag()] = tags
		allTags = append(allTags, tags...)
	}
	for _, tag := range allTags {
		if _, found := outboundMgr.Outbound(tag); !found {
			assert.Failf(t, "outbound missing from outbound manager", "outbound %s not found", tag)
		}
	}
	for group, tags := range want {
		assert.ElementsMatchf(t, tags, got[group], "group %s does not have correct outbounds", group)
	}
}

func getGroups(outboundMgr sbA.OutboundManager) []adapter.MutableOutboundGroup {
	outbounds := outboundMgr.Outbounds()
	var iGroups []adapter.MutableOutboundGroup
	for _, it := range outbounds {
		if group, isGroup := it.(adapter.MutableOutboundGroup); isGroup {
			iGroups = append(iGroups, group)
		}
	}
	return iGroups
}

func testEstablishConnection(t *testing.T, opts sbO.Options) {
	tmp := settings.GetString(settings.DataPathKey)

	opts.Route.RuleSet = baseOpts(settings.GetString(settings.DataPathKey)).Route.RuleSet
	opts.Route.RuleSet[0].LocalOptions.Path = filepath.Join(tmp, splitTunnelFile)
	opts.Route.Rules = append([]sbO.Rule{baseOpts(settings.GetString(settings.DataPathKey)).Route.Rules[2]}, opts.Route.Rules...)
	newSplitTunnel(tmp)

	err := establishConnection("", "", opts, tmp, nil)
	require.NoError(t, err, "failed to establish connection")
	t.Cleanup(func() {
		if tInstance != nil {
			tInstance.Close()
		}
	})

	assert.NotNil(t, tInstance, "tInstance should not be nil")
	assert.Equal(t, ipc.StatusRunning, tInstance.Status(), "tunnel should be running")
}
