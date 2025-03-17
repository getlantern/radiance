package boxservice

import (
	"context"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
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

	options := boxoptions.Options("temp.log")
	options.Outbounds = append(options.Outbounds, option.Outbound{Type: "direct", Tag: "testing-out"})
	bs, err := newlibbox(ctx, options, nil)
	require.NoError(t, err)
	require.NotNil(t, bs)

	ob := bs.instance.Outbound()
	_, fnd := ob.Outbound("testing-out")
	require.True(t, fnd, "outline-out not found")

	require.NoError(t, bs.Start())
	require.NoError(t, bs.Close())
}
