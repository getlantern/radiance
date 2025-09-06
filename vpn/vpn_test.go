package vpn

import (
	"context"
	"testing"

	sbx "github.com/getlantern/sing-box-extensions"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/vpn/ipc"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
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
	mservice := setupVpnTest(t)

	ctx := mservice.Ctx()
	clashServer := service.FromContext[adapter.ClashServer](ctx).(*clashapi.Server)
	outboundMgr := service.FromContext[adapter.OutboundManager](ctx)

	type gSelector interface {
		adapter.OutboundGroup
		Start() error
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// set initial group
			clashServer.SetMode(tt.initialGroup)

			// start the selector
			outbound, ok := outboundMgr.Outbound(tt.wantGroup)
			require.True(t, ok, tt.wantGroup+" selector should exist")
			selector := outbound.(gSelector)
			require.NoError(t, selector.Start(), "failed to start selector")

			mservice.status = ipc.StatusRunning
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
	_ = cacheFile.Close()

	t.Run("with tunnel closed", func(t *testing.T) {
		group, tag, err := selectedServer()
		require.NoError(t, err, "should not error when getting selected server")
		assert.Equal(t, wantGroup, group, "group should match")
		assert.Equal(t, wantTag, tag, "tag should match")
	})
	t.Run("with tunnel open", func(t *testing.T) {
		mservice := setupVpnTest(t)
		outboundMgr := service.FromContext[adapter.OutboundManager](mservice.Ctx())
		require.NoError(t, outboundMgr.Start(adapter.StartStateStart), "failed to start outbound manager")

		group, tag, err := selectedServer()
		require.NoError(t, err, "should not error when getting selected server")
		assert.Equal(t, wantGroup, group, "group should match")
		assert.Equal(t, wantTag, tag, "tag should match")
	})
}

type mockService struct {
	ctx    context.Context
	status string
	clash  *clashapi.Server
}

func (m *mockService) Ctx() context.Context          { return m.ctx }
func (m *mockService) Status() string                { return m.status }
func (m *mockService) ClashServer() *clashapi.Server { return m.clash }
func (m *mockService) Close() error                  { return nil }

func setupVpnTest(t *testing.T) *mockService {
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

	m := &mockService{
		ctx:    ctx,
		status: ipc.StatusInitializing,
		clash:  clashServer.(*clashapi.Server),
	}
	ipcServer = ipc.NewServer(m)
	t.Cleanup(func() {
		lb.Close()
		ipcServer.Close()
		cacheFile.Close()
		clashServer.Close()
	})
	require.NoError(t, ipcServer.Start(path))

	require.NoError(t, cacheFile.Start(adapter.StartStateInitialize))
	require.NoError(t, clashServer.Start(adapter.StartStateStart))
	return m
}
