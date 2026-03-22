package vpn

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"testing/synctest"

	box "github.com/getlantern/lantern-box"

	"github.com/getlantern/radiance/log"

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

	tmpDir := t.TempDir()
	client := setupVpnTest(t, tmpDir)

	ctx := client.tunnel.ctx
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

			require.NoError(t, client.SelectServer(tt.wantGroup, tt.wantTag))
			assert.Equal(t, tt.wantTag, selector.Now(), tt.wantTag+" should be selected")
			assert.Equal(t, tt.wantGroup, clashServer.Mode(), "clash mode should be "+tt.wantGroup)
		})
	}
}

func TestSelectedServer(t *testing.T) {
	wantGroup := "socks"
	wantTag := "socks2-out"

	tmpDir := t.TempDir()
	opts, _, err := testBoxOptions(tmpDir)
	require.NoError(t, err, "failed to load test box options")
	cacheFile := cachefile.New(context.Background(), *opts.Experimental.CacheFile)
	require.NoError(t, cacheFile.Start(adapter.StartStateInitialize))

	require.NoError(t, cacheFile.StoreMode(wantGroup))
	require.NoError(t, cacheFile.StoreSelected(wantGroup, wantTag))
	_ = cacheFile.Close()

	client := setupVpnTest(t, tmpDir)
	outboundMgr := service.FromContext[adapter.OutboundManager](client.tunnel.ctx)
	require.NoError(t, outboundMgr.Start(adapter.StartStateStart), "failed to start outbound manager")

	group, tag, err := client.GetSelected()
	require.NoError(t, err, "should not error when getting selected server")
	assert.Equal(t, wantGroup, group, "group should match")
	assert.Equal(t, wantTag, tag, "tag should match")
}

func TestAutoServerSelections(t *testing.T) {
	mgr := &mockOutMgr{
		outbounds: []adapter.Outbound{
			&mockOutbound{tag: "socks1-out"},
			&mockOutbound{tag: "socks2-out"},
			&mockOutbound{tag: "http1-out"},
			&mockOutbound{tag: "http2-out"},
			&mockOutboundGroup{
				mockOutbound: mockOutbound{tag: AutoLanternTag},
				now:          "socks1-out",
				all:          []string{"socks1-out", "socks2-out"},
			},
			&mockOutboundGroup{
				mockOutbound: mockOutbound{tag: AutoUserTag},
				now:          "http2-out",
				all:          []string{"http1-out", "http2-out"},
			},
			&mockOutboundGroup{
				mockOutbound: mockOutbound{tag: AutoSelectTag},
				now:          AutoLanternTag,
				all:          []string{AutoLanternTag, AutoUserTag},
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

	client := &VPNClient{
		tunnel: &tunnel{
			ctx: ctx,
		},
		logger: slog.Default(),
	}
	client.tunnel.status.Store(Connected)

	got, err := client.AutoServerSelections()
	require.NoError(t, err, "should not error when getting auto server selections")
	require.Equal(t, want, got, "selections should match")
}

func TestConnectWaitsForPreStartTests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, client := newIdleClient(true)
		go func() {
			<-ctx.Done()
			close(client.preTestDone)
		}()

		// Connect should block until pre-start tests complete (done channel closed).
		_ = client.Connect(BoxOptions{})
		<-client.preTestDone
	})
}

func TestConnectProceedsWithoutPreTests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, client := newIdleClient(false)
		_ = client.Connect(BoxOptions{})
	})
}

func TestStatusNotBlockedDuringPreTestWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, client := newIdleClient(true)
		go func() {
			_ = client.Connect(BoxOptions{})
		}()

		// Wait until the Connect goroutine is blocked on <-testDone (lock released).
		synctest.Wait()

		// Status should succeed because Connect released the write lock.
		assert.Equal(t, Disconnected, client.Status())
		close(client.preTestDone)
	})
}

// func TestConcurrentPreStartTestsRejected(t *testing.T) {
// 	_, client := newIdleClient(true)
// 	err := client.PreStartTests("", nil)
// 	require.Error(t, err)
// 	assert.Contains(t, err.Error(), "pre-start tests already running")
// }
//
// func TestPreStartTestsRejectedWhenConnected(t *testing.T) {
// 	_, client := newIdleClient(false)
// 	client.tunnel = &tunnel{}
//
// 	err := client.PreStartTests("", nil)
// 	assert.ErrorIs(t, err, ErrTunnelAlreadyConnected)
// }

func TestDisconnectedOperations(t *testing.T) {
	_, client := newIdleClient(false)

	assert.Equal(t, Disconnected, client.Status())
	assert.False(t, client.isOpen())
	assert.ErrorIs(t, client.SelectServer("g", "t"), ErrTunnelNotConnected)

	_, _, err := client.GetSelected()
	assert.ErrorIs(t, err, ErrTunnelNotConnected)

	_, _, err = client.ActiveServer()
	assert.ErrorIs(t, err, ErrTunnelNotConnected)

	_, err = client.Connections()
	assert.ErrorIs(t, err, ErrTunnelNotConnected)

	assert.NoError(t, client.Close(), "Close on disconnected client should be no-op")
	assert.NoError(t, client.Disconnect(), "Disconnect on disconnected client should be no-op")
}

// Run with -race
func TestConcurrentReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, client := newIdleClient(false)
		for range 10 {
			go func() {
				for range 100 {
					assert.Equal(t, Disconnected, client.Status())
				}
			}()
		}
	})
}

// Run with -race
func TestConcurrentConnectAndReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, client := newIdleClient(false)
		go func() {
			for range 10 {
				_ = client.Connect(BoxOptions{})
			}
		}()
		for range 5 {
			go func() {
				for range 50 {
					_ = client.Status()
				}
			}()
		}
	})
}

func newIdleClient(withPretests bool) (context.Context, *VPNClient) {
	done := make(chan struct{})
	ctx := context.Background()
	cancel := func() {}
	if withPretests {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		close(done)
	}
	return ctx, &VPNClient{
		logger:        log.NoOpLogger(),
		preTestCancel: cancel,
		preTestDone:   done,
	}
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

func setupVpnTest(t *testing.T, path string) *VPNClient {
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

	client := &VPNClient{
		tunnel: &tunnel{
			ctx:         ctx,
			clashServer: clashServer.(*clashapi.Server),
			dataPath:    path,
		},
		logger: slog.Default(),
	}
	client.tunnel.status.Store(Connected)

	t.Cleanup(func() {
		lb.Close()
		cacheFile.Close()
		clashServer.Close()
	})
	require.NoError(t, cacheFile.Start(adapter.StartStateInitialize))
	require.NoError(t, clashServer.Start(adapter.StartStateStart))
	return client
}
