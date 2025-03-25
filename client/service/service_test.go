package boxservice

import (
	"context"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/client/boxoptions"
	"github.com/getlantern/radiance/protocol"
)

func TestSelectCustomServer(t *testing.T) {
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	options := boxoptions.Options("stderr")
	options.Outbounds = append(options.Outbounds, option.Outbound{Type: "direct", Tag: "testing-out"})

	cfg, err := json.MarshalContext(ctx, boxoptions.BoxOptions)
	require.NoError(t, err)
	bs, err := New(string(cfg), "", "", nil)
	require.NoError(t, err)
	require.NotNil(t, bs)

	// If we're adding an endpoint with wireguard, a wireguard inbound is required
	customConfig := `{
		"outbounds": [
			{
				"type": "algeneva",
				"tag": "algeneva-out",
				"server": "103.104.245.192",
				"server_port": 80,
				"headers": {
					"x-auth-token": "token"
				},
				"tls": {
					"enabled": true,
					"disable_sni": false,
					"server_name": "",
					"insecure": false,
					"min_version": "",
					"max_version": "",
					"cipher_suites": [],
					"certificate": ""
				},
				"strategy": "[HTTP:method:*]-insert{%0A:end:value:4}-|"
			}
		],
		"route": {
			"rules": [
				{
					"inbound": "tun-in",
					"action": "route",
					"outbound": "algeneva-out"
				}
			]
		} 
	}`

	t.Run("it should successfully add algeneva outbound", func(t *testing.T) {
		err = bs.AddCustomServer("algeneva-out", []byte(customConfig))
		assert.NoError(t, err)

		// checking if algeneva-out was included as an outbound and route
		outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
		_, exists := outboundManager.Outbound("algeneva-out")
		assert.True(t, exists)
	})

	t.Run("it should remove the outbound tag", func(t *testing.T) {
		err = bs.RemoveCustomServer("algeneva-out")
		assert.NoError(t, err)
		outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
		_, exists := outboundManager.Outbound("algeneva-out")
		assert.False(t, exists)
	})
}
