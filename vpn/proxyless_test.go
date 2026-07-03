package vpn

import (
	"testing"

	lbC "github.com/getlantern/lantern-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
)

// The server sends the proxyless rule-set detour as a normal outbound. It must
// be merged into the box config (so download_detour: "proxyless" resolves) but
// excluded from the user-selectable proxy groups.
func TestMergeAndCollectTags_ExcludesProxylessDetour(t *testing.T) {
	dst := &O.Options{Route: &O.RouteOptions{}}
	src := &O.Options{
		Outbounds: []O.Outbound{
			{Type: "shadowsocks", Tag: "ss-out-proxy1"},
			{Type: lbC.TypeOutline, Tag: proxylessDetourTag},
		},
	}

	tags := mergeAndCollectTags(dst, src)

	assert.Contains(t, tags, "ss-out-proxy1", "a real proxy should be selectable")
	assert.NotContains(t, tags, proxylessDetourTag, "the proxyless detour must not be user-selectable")

	merged := false
	for _, o := range dst.Outbounds {
		if o.Tag == proxylessDetourTag {
			merged = true
		}
	}
	assert.True(t, merged, "the proxyless outbound must still be merged into the config so the detour resolves")
}
