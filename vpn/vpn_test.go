package vpn

import (
	"context"
	"testing"

	sbx "github.com/getlantern/sing-box-extensions"

	"github.com/getlantern/radiance/common"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectServer(t *testing.T) {
	var tests = []struct {
		name         string
		initialGroup string
		wantGroup    string
		wantTag      string
	}{
		{
			name:         "select in same group",
			initialGroup: "socks",
			wantGroup:    "socks",
			wantTag:      "socks2-out",
		},
		{
			name:         "select in different group",
			initialGroup: "socks",
			wantGroup:    "http",
			wantTag:      "http2-out",
		},
	}

	common.SetPathsForTesting(t)
	ctx := setupVpnTest(t)

	clashServer := service.FromContext[adapter.ClashServer](ctx).(*clashapi.Server)
	outboundMgr := service.FromContext[adapter.OutboundManager](ctx)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// set initial group
			clashServer.SetMode(tt.initialGroup)

			// start the selector
			outbound, ok := outboundMgr.Outbound(tt.wantGroup)
			require.True(t, ok, tt.wantGroup+" selector should exist")
			selector := outbound.(*group.Selector)
			require.NoError(t, selector.Start(), "failed to start selector")

			require.NoError(t, selectServer(tt.wantGroup, tt.wantTag))
			assert.Equal(t, tt.wantTag, selector.Now(), tt.wantTag+" should be selected")
			assert.Equal(t, tt.wantGroup, clashServer.Mode(), "clash mode should be "+tt.wantGroup)
		})
	}
}

func TestSelectedServer(t *testing.T) {
	wantGroup := "socks"
	wantTag := "socks2-out"

	common.SetPathsForTesting(t)
	opts, _, err := testBoxOptions(common.DataPath())
	require.NoError(t, err, "failed to load test box options")
	cacheFile := cachefile.New(context.Background(), *opts.Experimental.CacheFile)
	require.NoError(t, cacheFile.Start(adapter.StartStateInitialize))

	require.NoError(t, cacheFile.StoreMode(wantGroup))
	require.NoError(t, cacheFile.StoreSelected(wantGroup, wantTag))
	err = cacheFile.Close()

	t.Run("with tunnel closed", func(t *testing.T) {
		group, tag, err := selectedServer()
		require.NoError(t, err, "should not error when getting selected server")
		assert.Equal(t, wantGroup, group, "group should match")
		assert.Equal(t, wantTag, tag, "tag should match")
	})
	t.Run("with tunnel open", func(t *testing.T) {
		ctx := setupVpnTest(t)
		outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
		require.NoError(t, outboundMgr.Start(adapter.StartStateStart), "failed to start outbound manager")

		group, tag, err := selectedServer()
		require.NoError(t, err, "should not error when getting selected server")
		assert.Equal(t, wantGroup, group, "group should match")
		assert.Equal(t, wantTag, tag, "tag should match")
	})
}

func TestIsOpened(t *testing.T) {
	common.SetPathsForTesting(t)
	assert.False(t, isOpen(), "tunnel should not be open")
	path := common.DataPath()
	setupOpts := libbox.SetupOptions{
		BasePath:    path,
		WorkingPath: path,
		TempPath:    path,
	}
	require.NoError(t, libbox.Setup(&setupOpts))
	require.NoError(t, startCmdServer())
	require.True(t, isOpen(), "tunnel should be open after starting command server")
}

func TestSendCmd(t *testing.T) {
	common.SetPathsForTesting(t)
	ctx := setupVpnTest(t)

	clashServer := service.FromContext[adapter.ClashServer](ctx).(*clashapi.Server)
	want := clashServer.Mode()

	res, err := sendCmd(libbox.CommandClashMode)
	require.NoError(t, err)
	require.Equal(t, want, res.clashMode)
}

func setupVpnTest(t *testing.T) context.Context {
	path := common.DataPath()
	setupOpts := libbox.SetupOptions{
		BasePath:    path,
		WorkingPath: path,
		TempPath:    path,
	}
	require.NoError(t, libbox.Setup(&setupOpts))

	_, boxOpts, err := testBoxOptions(path)
	require.NoError(t, err, "failed to load test box options")

	ctx := sbx.BoxContext()

	lb, err := libbox.NewServiceWithContext(ctx, boxOpts, nil)
	require.NoError(t, err)
	clashServer := service.FromContext[adapter.ClashServer](ctx)
	cacheFile := service.FromContext[adapter.CacheFile](ctx)

	cmdSvr = libbox.NewCommandServer(&cmdSvrHandler{}, 64)
	t.Cleanup(func() {
		lb.Close()
		cmdSvr.Close()
	})

	require.NoError(t, cmdSvr.Start())
	cmdSvr.SetService(lb)

	require.NoError(t, cacheFile.Start(adapter.StartStateInitialize))
	require.NoError(t, clashServer.Start(adapter.StartStateStart))

	return ctx
}
