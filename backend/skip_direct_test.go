package backend

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/common/settings"
)

func TestWithoutDirectRules(t *testing.T) {
	direct := C.SmartRoutingRule{Category: "direct", Outbounds: []string{"direct"}}
	proxied := C.SmartRoutingRule{Category: "streaming", Outbounds: []string{"srv-a", "srv-b"}}
	mixed := C.SmartRoutingRule{Category: "mixed", Outbounds: []string{"direct", "srv-a"}}
	rules := C.SmartRoutingRules{direct, proxied, mixed}

	got := withoutDirectRules(rules)
	assert.Equal(t, C.SmartRoutingRules{proxied, mixed}, got, "only rules that go only direct are dropped")
	assert.Equal(t, C.SmartRoutingRules{direct, proxied, mixed}, rules, "the config's rules are left as they were")
	assert.Empty(t, withoutDirectRules(C.SmartRoutingRules{direct}))
}

// With the setting on, the tunnel gets no direct rules but keeps the
// proxyless hedge, so sites a direct list covers go through it.
func TestSkipDirectRulesSetting(t *testing.T) {
	cfg := cachedConfig()
	cfg.Features = map[string]bool{proxylessFeature: true}
	cfg.SmartRouting = C.SmartRoutingRules{{
		Category:  "direct",
		Outbounds: []string{"direct"},
		RuleSets:  C.RuleSets{{Tag: "smart-routing-us-common-direct", URL: "https://example.com/common.srs"}},
	}}
	r := proxylessBackend(t, t.TempDir(), cfg)

	opts := r.getBoxOptions()
	require.Len(t, opts.SmartRouting, 1, "off by default: the direct rule stays")

	require.NoError(t, settings.Set(settings.SmartRoutingSkipDirectKey, true))
	opts = r.getBoxOptions()
	assert.Empty(t, opts.SmartRouting, "on: the direct rule is dropped")
	require.NotNil(t, opts.Proxyless, "the hedge is still built")
	assert.True(t, opts.Proxyless.Enabled)
}
