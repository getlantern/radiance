package vpn

import (
	"context"
	"slices"
	"testing"

	box "github.com/getlantern/lantern-box"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
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

	type _selector interface {
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
			selector := outbound.(_selector)
			require.NoError(t, selector.Start(), "failed to start selector")

			mservice.status = ipc.StatusRunning
			require.NoError(t, selectServer(context.Background(), tt.wantGroup, tt.wantTag))
			assert.Equal(t, tt.wantTag, selector.Now(), tt.wantTag+" should be selected")
			assert.Equal(t, tt.wantGroup, clashServer.Mode(), "clash mode should be "+tt.wantGroup)
		})
	}
}

func TestSelectedServer(t *testing.T) {
	wantGroup := "socks"
	wantTag := "socks2-out"

	common.SetPathsForTesting(t)
	opts, _, err := testBoxOptions(settings.GetString(settings.DataPathKey))
	require.NoError(t, err, "failed to load test box options")
	cacheFile := cachefile.New(context.Background(), *opts.Experimental.CacheFile)
	require.NoError(t, cacheFile.Start(adapter.StartStateInitialize))

	require.NoError(t, cacheFile.StoreMode(wantGroup))
	require.NoError(t, cacheFile.StoreSelected(wantGroup, wantTag))
	_ = cacheFile.Close()

	t.Run("with tunnel open", func(t *testing.T) {
		mservice := setupVpnTest(t)
		outboundMgr := service.FromContext[adapter.OutboundManager](mservice.Ctx())
		require.NoError(t, outboundMgr.Start(adapter.StartStateStart), "failed to start outbound manager")

		group, tag, err := ipc.GetSelected(context.Background())
		require.NoError(t, err, "should not error when getting selected server")
		assert.Equal(t, wantGroup, group, "group should match")
		assert.Equal(t, wantTag, tag, "tag should match")
	})
}

func TestAutoServerSelections(t *testing.T) {
	common.SetPathsForTesting(t)
	mgr := &mockOutMgr{
		outbounds: []adapter.Outbound{
			&mockOutbound{tag: "socks1-out"},
			&mockOutbound{tag: "socks2-out"},
			&mockOutbound{tag: "http1-out"},
			&mockOutbound{tag: "http2-out"},
			&mockOutboundGroup{
				mockOutbound: mockOutbound{tag: autoLanternTag},
				now:          "socks1-out",
				all:          []string{"socks1-out", "socks2-out"},
			},
			&mockOutboundGroup{
				mockOutbound: mockOutbound{tag: autoUserTag},
				now:          "http2-out",
				all:          []string{"http1-out", "http2-out"},
			},
			&mockOutboundGroup{
				mockOutbound: mockOutbound{tag: autoAllTag},
				now:          autoLanternTag,
				all:          []string{autoLanternTag, autoUserTag},
			},
		},
	}
	want := AutoSelections{
		Lantern: "socks1-out",
		User:    "http2-out",
		AutoAll: "socks1-out",
	}
	ctx := box.BaseContext()
	service.MustRegister[adapter.OutboundManager](ctx, mgr)
	m := &mockService{
		ctx:    ctx,
		status: ipc.StatusRunning,
	}
	ipcServer = ipc.NewServer(m)
	require.NoError(t, ipcServer.Start(settings.GetString(settings.DataPathKey)))

	got, err := AutoServerSelections()
	require.NoError(t, err, "should not error when getting auto server selections")
	require.Equal(t, want, got, "selections should match")
}

type mockOutMgr struct {
	adapter.OutboundManager
	outbounds []adapter.Outbound
}

func (o *mockOutMgr) Outbounds() []adapter.Outbound {
	return o.outbounds
}

func (o *mockOutMgr) Outbound(tag string) (adapter.Outbound, bool) {
	idx := slices.IndexFunc(o.outbounds, func(ob adapter.Outbound) bool {
		return ob.Tag() == tag
	})
	if idx == -1 {
		return nil, false
	}
	return o.outbounds[idx], true
}

type mockOutbound struct {
	adapter.Outbound
	tag string
}

func (o *mockOutbound) Tag() string  { return o.tag }
func (o *mockOutbound) Type() string { return "mock" }

type mockOutboundGroup struct {
	mockOutbound
	now string
	all []string
}

func (o *mockOutboundGroup) Now() string   { return o.now }
func (o *mockOutboundGroup) All() []string { return o.all }

var _ ipc.Service = (*mockService)(nil)

type mockService struct {
	ctx    context.Context
	status string
	clash  *clashapi.Server
}

func (m *mockService) Ctx() context.Context                               { return m.ctx }
func (m *mockService) Status() string                                     { return m.status }
func (m *mockService) ClashServer() *clashapi.Server                      { return m.clash }
func (m *mockService) Close() error                                       { return nil }
func (m *mockService) Start(ctx context.Context, group, tag string) error { return nil }
func (m *mockService) Restart(ctx context.Context) error                  { return nil }

func setupVpnTest(t *testing.T) *mockService {
	path := settings.GetString(settings.DataPathKey)
	setupOpts := libbox.SetupOptions{
		BasePath:    path,
		WorkingPath: path,
		TempPath:    path,
	}
	require.NoError(t, libbox.Setup(&setupOpts))

	_, boxOpts, err := testBoxOptions(path)
	require.NoError(t, err, "failed to load test box options")

	ctx := box.BaseContext()

	lb, err := libbox.NewServiceWithContext(ctx, boxOpts, nil)
	require.NoError(t, err)
	clashServer := service.FromContext[adapter.ClashServer](ctx)
	cacheFile := service.FromContext[adapter.CacheFile](ctx)

	m := &mockService{
		ctx:    ctx,
		status: ipc.StatusRunning,
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
