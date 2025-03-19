package boxservice

import (
	"context"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/client/boxoptions"
	"github.com/getlantern/radiance/protocol"
)

func TestNewlibbox(t *testing.T) {
	// TODO: expand this to test that libbox is actually using the provided context, options, and
	// platformInterface
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	options := boxoptions.Options("stderr")
	options.Outbounds = append(options.Outbounds, option.Outbound{Type: "direct", Tag: "testing-out"})
	bs, err := configureLibboxService(ctx, options, nil)
	require.NoError(t, err)
	require.NotNil(t, bs)

	ob := bs.instance.Outbound()
	_, fnd := ob.Outbound("testing-out")
	require.True(t, fnd, "testing-out not found")
	// TODO: use custom box options that don't need sudo to test this (i.e. no TUN).
	// for now, just test that it doesn't panic
	require.NotPanics(t, func() { bs.Start() })
	bs.Close()
}

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
	bs, err := newlibbox(ctx, options, nil)
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
		err = bs.SelectCustomServer([]byte(customConfig))
		assert.NoError(t, err)

		// making sure default options haven't been changed
		assert.Equal(t, options, bs.defaultOptions)

		// checking if algeneva-out was included as an outbound and route
		_, exists := bs.instance.Outbound().Outbound("algeneva-out")
		assert.True(t, exists)
		routingRules := bs.instance.Router().Rules()
		assert.Equal(t, "route(algeneva-out)", routingRules[len(routingRules)-2].Action().String())
	})

	t.Run("it should de-select the server and use the default provided instance", func(t *testing.T) {
		err = bs.DeselectCustomServer()
		assert.NoError(t, err)
		// making sure default options haven't been changed
		assert.Equal(t, options, bs.defaultOptions)
		_, exists := bs.instance.Outbound().Outbound("algeneva-out")
		assert.False(t, exists)
	})
}
