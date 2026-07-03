package vpn

import (
	"testing"

	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
)

// The server declares certain outbounds (e.g. a proxyless rule-set detour) as
// non-selectable via NonSelectableOutbounds. They must be merged into the box
// config so their references (download_detour) resolve, but excluded from the
// user-selectable proxy groups.
func TestMergeAndCollectTags_ExcludesNonSelectable(t *testing.T) {
	dst := &O.Options{Route: &O.RouteOptions{}}
	src := &O.Options{
		Outbounds: []O.Outbound{
			{Type: "shadowsocks", Tag: "ss-out-proxy1"},
			{Type: "outline", Tag: "proxyless"}, // server-declared non-selectable
			{Type: "direct", Tag: "direct"},     // base reserved tag
		},
	}

	tags := mergeAndCollectTags(dst, src, []string{"proxyless"})

	assert.Contains(t, tags, "ss-out-proxy1", "a real proxy should be selectable")
	assert.NotContains(t, tags, "proxyless", "a server-declared non-selectable outbound must be excluded")
	assert.NotContains(t, tags, "direct", "a base reserved tag must be excluded")

	merged := 0
	for _, o := range dst.Outbounds {
		if o.Tag == "proxyless" {
			merged++
		}
	}
	assert.Equal(t, 1, merged, "the non-selectable outbound must still be merged into the config")
}
