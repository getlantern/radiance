package boxservice

import (
	"testing"
)

func TestNewlibbox(t *testing.T) {
	/*
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
  */
}
