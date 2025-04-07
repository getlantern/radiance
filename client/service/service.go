/*
Package boxservice provides a wrapper around libbox.BoxService, managing network control,
state handling, and platform-specific behavior. It supports functionalities such as
network pausing and resuming.

This package is designed for both desktop and mobile platforms, with mobile-specific
platform interfaces being handled internally.
*/
package boxservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	C "github.com/getlantern/common"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"github.com/getlantern/sing-box-extensions/ruleset"

	"github.com/getlantern/radiance/protocol"
)

// BoxService is a wrapper around libbox.BoxService
type BoxService struct {
	libbox            *libbox.BoxService
	ctx               context.Context
	config            string
	platIfce          libbox.PlatformInterface
	mutRuleSetManager *ruleset.Manager
	pauseManager      pause.Manager
	pauseAccess       sync.Mutex
	pauseTimer        *time.Timer
	mu                sync.Mutex
	isRunning         bool
}

// New creates a new BoxService that wraps a [libbox.BoxService]. platformInterface is used
// to interact with the underlying platform
func New(config, dataDir string, platIfce libbox.PlatformInterface, rulesetManager *ruleset.Manager) (*BoxService, error) {
	bs := &BoxService{
		config:            config,
		platIfce:          platIfce,
		mutRuleSetManager: rulesetManager,
	}
	setupOpts := &libbox.SetupOptions{
		BasePath:    dataDir,
		WorkingPath: filepath.Join(dataDir, "data"),
		TempPath:    filepath.Join(dataDir, "temp"),
	}
	if runtime.GOOS == "android" {
		setupOpts.FixAndroidStack = true
	}
	if err := libbox.Setup(setupOpts); err != nil {
		return nil, fmt.Errorf("setup libbox: %w", err)
	}

	// (re)-initialize the libbox service
	lb, ctx, err := newLibboxService(bs.config, bs.platIfce)
	if err != nil {
		return nil, err
	}
	ctx = service.ContextWithPtr(ctx, bs.mutRuleSetManager)
	bs.libbox = lb
	bs.ctx = ctx

	bs.pauseManager = service.FromContext[pause.Manager](ctx)

	return bs, nil
}

func (bs *BoxService) Start() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if bs.isRunning {
		return errors.New("service is already running")
	}

	// we need to start the ruleset manager before starting the libbox service but after the libbox
	// service has been initialized so that the ruleset manager can access the routing rules.
	if err := bs.mutRuleSetManager.Start(bs.ctx); err != nil {
		return fmt.Errorf("start ruleset manager: %w", err)
	}

	if err := bs.libbox.Start(); err != nil {
		return fmt.Errorf("error starting libbox service: %w", err)
	}
	bs.isRunning = true
	return nil
}

// newLibboxService creates a new libbox service with the given config and platform interface
func newLibboxService(config string, platIfce libbox.PlatformInterface) (*libbox.BoxService, context.Context, error) {
	// Retrieve protocol registries
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	// initialize the libbox service
	lb, err := libbox.NewServiceWithContext(ctx, config, platIfce)
	if err != nil {
		return nil, nil, fmt.Errorf("create libbox service: %w", err)
	}

	return lb, ctx, nil
}

func (bs *BoxService) Close() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if !bs.isRunning {
		return errors.New("service already stopped")
	}

	// Clear pause timer
	if bs.pauseTimer != nil {
		bs.pauseTimer.Stop()
		bs.pauseTimer = nil
	}

	if bs.libbox != nil {
		err := bs.libbox.Close()
		if err != nil {
			return fmt.Errorf("failed to close libbox: %v", err)
		}
		bs.libbox = nil
	}

	bs.isRunning = false
	return nil
}

// Pause pauses the network for the specified duration. An error is returned if the network is
// already paused
func (bs *BoxService) Pause(dur time.Duration) error {
	bs.pauseAccess.Lock()
	defer bs.pauseAccess.Unlock()

	if bs.pauseManager.IsNetworkPaused() {
		return errors.New("network is already paused")
	}

	bs.pauseManager.NetworkPause()
	bs.pauseTimer = time.AfterFunc(dur, bs.Wake)
	return nil
}

// Wake unpause the network. If the network is not paused, this function does nothing
func (bs *BoxService) Wake() {
	bs.pauseAccess.Lock()
	defer bs.pauseAccess.Unlock()

	if !bs.pauseManager.IsNetworkPaused() {
		return
	}
	bs.pauseManager.NetworkWake()
	if bs.pauseTimer != nil {
		bs.pauseTimer.Stop()
		bs.pauseTimer = nil
	}
}

func (bs *BoxService) Ctx() context.Context {
	return bs.ctx
}

// OnNewConfig is called when a new configuration is received. It updates the VPN client with the
// new configuration and restarts the VPN client if necessary.
func (bs *BoxService) OnNewConfig(oldConfigRaw, newConfigRaw []byte) error {
	slog.Debug("Received new config")

	_, err := json.UnmarshalExtendedContext[*C.ConfigResponse](bs.ctx, oldConfigRaw)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	_, err = json.UnmarshalExtendedContext[*C.ConfigResponse](bs.ctx, oldConfigRaw)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	return nil
}
