package vpn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/x/list"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"
)

type resetMonitor struct {
	tun.DefaultInterfaceMonitor
	closed func()
}

func (*resetMonitor) Start() error { return nil }
func (m *resetMonitor) Close() error {
	if m.closed != nil {
		m.closed()
	}
	return nil
}
func (*resetMonitor) RegisterCallback(tun.DefaultInterfaceUpdateCallback) *list.Element[tun.DefaultInterfaceUpdateCallback] {
	return nil
}
func (*resetMonitor) UpdateDefaultInterface(string, int32, bool, bool) {}

type resetPlatform struct {
	adapter.PlatformInterface
	monitor *resetMonitor
}

func (*resetPlatform) Initialize(adapter.NetworkManager) error { return nil }
func (p *resetPlatform) CreateDefaultInterfaceMonitor(logger.Logger) tun.DefaultInterfaceMonitor {
	return p.monitor
}
func (*resetPlatform) UsePlatformNetworkInterfaces() bool { return true }
func (*resetPlatform) NetworkInterfaces() ([]adapter.NetworkInterface, error) {
	return nil, nil
}

type resetRouter struct {
	adapter.Router
	resets  atomic.Int32
	onReset func()
	monitor *resetMonitor
}

func (r *resetRouter) ResetNetwork() {
	r.resets.Add(1)
	if r.onReset != nil {
		r.onReset()
	}
}

func resetTestTunnel(t *testing.T) (*tunnel, *resetRouter) {
	t.Helper()
	p := &resetPlatform{monitor: &resetMonitor{}}
	ctx := service.ContextWith[adapter.PlatformInterface](box.Context(context.Background()), p)
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: option.Options{
		Log: &option.LogOptions{Disabled: true},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, instance.Close()) })
	router := &resetRouter{monitor: p.monitor}
	service.MustRegister[adapter.Router](ctx, router)
	require.NoError(t, instance.Network().Start(adapter.StartStateInitialize))
	return &tunnel{boxInstance: instance}, router
}

func TestResetNetworkForMonitor(t *testing.T) {
	tn, router := resetTestTunnel(t)
	client := &VPNClient{tunnel: tn}
	client.ResetNetworkForMonitor(nil)
	client.ResetNetworkForMonitor(&resetMonitor{})
	require.Zero(t, router.resets.Load())

	client.ResetNetworkForMonitor(router.monitor)
	require.EqualValues(t, 1, router.resets.Load())
	client.ResetNetwork()
	require.EqualValues(t, 2, router.resets.Load())

	client.tunnel = nil
	client.ResetNetworkForMonitor(router.monitor)
	client.tunnel = &tunnel{}
	client.ResetNetworkForMonitor(router.monitor)
	require.EqualValues(t, 2, router.resets.Load())
}

func TestResetNetworkForMonitorRejectsReplacementAfterWaitingForLock(t *testing.T) {
	oldTunnel, oldRouter := resetTestTunnel(t)
	newTunnel, newRouter := resetTestTunnel(t)
	client := &VPNClient{tunnel: oldTunnel}

	client.mu.Lock()
	unlock := sync.OnceFunc(client.mu.Unlock)
	t.Cleanup(unlock)
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		client.ResetNetworkForMonitor(oldRouter.monitor)
		close(done)
	}()
	<-started
	select {
	case <-done:
		t.Fatal("reset did not wait for the tunnel lock")
	case <-time.After(20 * time.Millisecond):
	}
	client.tunnel = newTunnel
	unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reset did not finish after tunnel replacement")
	}
	require.Zero(t, oldRouter.resets.Load())
	require.Zero(t, newRouter.resets.Load())
}

func TestResetNetworkForMonitorHoldsTunnelUntilResetCompletes(t *testing.T) {
	oldTunnel, oldRouter := resetTestTunnel(t)
	newTunnel, _ := resetTestTunnel(t)
	client := &VPNClient{tunnel: oldTunnel}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	oldRouter.onReset = func() {
		close(entered)
		<-release
	}
	go func() {
		client.ResetNetworkForMonitor(oldRouter.monitor)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reset did not reach the current tunnel")
	}
	locked := client.mu.TryLock()
	if locked {
		client.mu.Unlock()
	}
	require.False(t, locked, "replacement must wait for reset to finish")
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reset did not finish")
	}
	client.mu.Lock()
	client.tunnel = newTunnel
	client.mu.Unlock()
}

func TestResetNetworkForMonitorAllowsMonitorCallbacks(t *testing.T) {
	tn, router := resetTestTunnel(t)
	client := &VPNClient{tunnel: tn}
	callbackRan := make(chan struct{})
	var callbackOnce sync.Once
	router.monitor.closed = func() { callbackOnce.Do(func() { close(callbackRan) }) }
	router.onReset = func() { _ = router.monitor.Close() }
	done := make(chan struct{})
	go func() {
		client.ResetNetworkForMonitor(router.monitor)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reset blocked a monitor callback")
	}
	select {
	case <-callbackRan:
	default:
		t.Fatal("reset did not invoke the monitor callback")
	}
	require.EqualValues(t, 1, router.resets.Load())
}
