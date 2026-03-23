package vpn

import (
	"path/filepath"
	"testing"
	"time"

	sbA "github.com/sagernet/sing-box/adapter"
	sbC "github.com/sagernet/sing-box/constant"
	sbO "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"

	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
)

func TestConnection(t *testing.T) {
	tmp := t.TempDir()
	opts, optsStr, err := testBoxOptions(tmp)
	require.NoError(t, err, "failed to get test box options")

	baseOptions := baseOpts(tmp)
	opts.Route.RuleSet = baseOptions.Route.RuleSet
	opts.Route.RuleSet[0].LocalOptions.Path = filepath.Join(tmp, splitTunnelFile)
	opts.Route.Rules = append([]sbO.Rule{baseOptions.Route.Rules[2]}, opts.Route.Rules...)
	newSplitTunnel(tmp, log.NoOpLogger())

	tun := &tunnel{
		dataPath: tmp,
	}

	require.NoError(t, tun.start(optsStr, nil), "failed to establish connection")
	t.Cleanup(func() {
		tun.close()
	})

	require.Equal(t, Connected, tun.Status(), "tunnel should be running")

	assert.NoError(t, tun.selectOutbound("http", "http1-out"), "failed to select http outbound")
	assert.NoError(t, tun.close(), "failed to close lbService")
	assert.Equal(t, Disconnected, tun.Status(), "tun should be closed")
}

func TestUpdateServers(t *testing.T) {
	tmp := t.TempDir()
	testOpts, _, err := testBoxOptions(tmp)
	require.NoError(t, err, "failed to get test box options")

	baseOptions := baseOpts(tmp)
	baseOuts := baseOptions.Outbounds
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
		urlTestOutbound(AutoLanternTag, lanternTags, nil), urlTestOutbound(AutoUserTag, userTags, nil),
		selectorOutbound(servers.SGLantern, append(lanternTags, AutoLanternTag)),
		selectorOutbound(servers.SGUser, append(userTags, AutoUserTag)),
		urlTestOutbound(AutoSelectTag, []string{AutoLanternTag, AutoUserTag}, nil),
	}

	testOpts.Outbounds = outs
	testOpts.Route.RuleSet = baseOptions.Route.RuleSet
	testOpts.Route.RuleSet[0].LocalOptions.Path = filepath.Join(tmp, splitTunnelFile)
	testOpts.Route.Rules = append([]sbO.Rule{baseOptions.Route.Rules[2]}, testOpts.Route.Rules...)
	newSplitTunnel(tmp, log.NoOpLogger())

	tun := &tunnel{
		dataPath: tmp,
	}
	options, _ := json.Marshal(testOpts)
	err = tun.start(string(options), nil)
	require.NoError(t, err, "failed to establish connection")
	t.Cleanup(func() {
		tun.close()
	})

	assert.Equal(t, Connected, tun.Status(), "tunnel should be running")
	defer func() {
		tun.close()
	}()

	time.Sleep(500 * time.Millisecond)

	err = tun.removeOutbounds(servers.SGLantern, []string{"http2-out", "socks1-out"})
	require.NoError(t, err, "failed to remove servers from lantern")

	newOpts := servers.Options{
		Outbounds: []sbO.Outbound{
			allOutbounds["http1-out"], allOutbounds["socks2-out"],
		},
	}
	err = tun.addOutbounds(servers.SGLantern, newOpts)
	require.NoError(t, err, "failed to update servers for lantern")

	time.Sleep(250 * time.Millisecond)

	outboundMgr := service.FromContext[sbA.OutboundManager](tun.ctx)
	require.NotNil(t, outboundMgr, "outbound manager should not be nil")

	groups := tun.mutGrpMgr.OutboundGroups()

	want := map[string][]string{
		AutoSelectTag:     {AutoLanternTag, AutoUserTag},
		servers.SGLantern: {AutoLanternTag, "http1-out", "socks2-out"},
		AutoLanternTag:    {"http1-out", "socks2-out"},
		servers.SGUser:    {AutoUserTag},
		AutoUserTag:       {},
	}
	got := make(map[string][]string)
	allTags := []string{"direct", "block", AutoSelectTag, AutoLanternTag, AutoUserTag, servers.SGLantern, servers.SGUser}
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
