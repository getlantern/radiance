package vpn

import (
	"testing"

	box "github.com/getlantern/lantern-box"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAndroidSplitTunnelCapture(t *testing.T) {
	for _, tt := range []struct {
		name             string
		state            SplitTunnelState
		include, exclude []string
	}{
		{"disabled", SplitTunnelState{Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{PackageName: []string{"selected.app"}}}, nil, nil},
		{"bypass", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyExclude, Filters: SplitTunnelFilter{PackageName: []string{"selected.app"}, DomainSuffix: []string{"example.com"}}}, nil, []string{"selected.app"}},
		{"include apps", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{PackageName: []string{"selected.app"}}}, []string{"selected.app"}, nil},
		{"include empty", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude}, []string{"@none"}, nil},
		{"include mixed", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{PackageName: []string{"selected.app"}, DomainSuffix: []string{"example.com"}}}, nil, nil},
		{"include website", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{Domain: []string{"example.com"}}}, nil, nil},
		{"include regex", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{DomainRegex: []string{"example"}}}, nil, nil},
		{"include keyword", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{DomainKeyword: []string{"example"}}}, nil, nil},
		{"include other app selector", SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{ProcessName: []string{"selected"}}}, nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			options := O.Options{Inbounds: []O.Inbound{{Type: "tun", Options: &O.TunInboundOptions{}}}}
			configureSplitTunnelCapture(options.Inbounds, tt.state, "android")
			ctx := box.Context(t.Context())
			encoded, err := json.MarshalContext(ctx, options)
			require.NoError(t, err)
			decoded, err := json.UnmarshalExtendedContext[O.Options](ctx, encoded)
			require.NoError(t, err)
			tun := decoded.Inbounds[0].Options.(*O.TunInboundOptions)
			assert.Equal(t, tt.include, []string(tun.IncludePackage))
			assert.Equal(t, tt.exclude, []string(tun.ExcludePackage))
		})
	}
}

func TestAndroidCaptureRequiresRebuild(t *testing.T) {
	apps := SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude, Filters: SplitTunnelFilter{PackageName: []string{"selected.app"}}}
	mixed := apps
	mixed.Filters.DomainSuffix = []string{"example.com"}
	moreSites := mixed
	moreSites.Filters.DomainSuffix = []string{"example.com", "another.example"}
	empty := SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude}
	bypass := apps
	bypass.Policy = SplitTunnelPolicyExclude
	otherApp := bypass
	otherApp.Filters.PackageName = []string{"other.app"}
	for _, change := range [][2]SplitTunnelState{{apps, mixed}, {mixed, apps}, {apps, empty}, {bypass, otherApp}, {empty, mixed}} {
		assert.True(t, splitTunnelCaptureChanged(change[0], change[1], "android"))
		assert.False(t, splitTunnelCaptureChanged(change[0], change[1], "darwin"))
	}
	assert.False(t, splitTunnelCaptureChanged(mixed, moreSites, "android"))
	assert.False(t, splitTunnelCaptureChanged(apps, apps, "android"))
	apps.Enabled, empty.Enabled = false, false
	assert.False(t, splitTunnelCaptureChanged(apps, empty, "android"))
}
