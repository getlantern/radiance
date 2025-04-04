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
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/group"
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

	logFactory         log.Factory
	customServersMutex sync.Locker
	customServers      map[string]option.Options
}

// New creates a new BoxService that wraps a [libbox.BoxService]. platformInterface is used
// to interact with the underlying platform
func New(config, dataDir string, platIfce libbox.PlatformInterface, rulesetManager *ruleset.Manager, logFactory log.Factory) (*BoxService, error) {
	bs := &BoxService{
		config:            config,
		platIfce:          platIfce,
		mutRuleSetManager: rulesetManager,
		logFactory:        logFactory,
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

	return bs, nil
}

func (bs *BoxService) Start() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if bs.isRunning {
		return errors.New("service is already running")
	}

	// (re)-initialize the libbox service
	lb, ctx, err := newLibboxService(bs.config, bs.platIfce)
	if err != nil {
		return err
	}

	// we need to start the ruleset manager before starting the libbox service but after the libbox
	// service has been initialized so that the ruleset manager can access the routing rules.
	if err = bs.mutRuleSetManager.Start(ctx); err != nil {
		return fmt.Errorf("start ruleset manager: %w", err)
	}
	ctx = service.ContextWithPtr(ctx, bs.mutRuleSetManager)

	bs.libbox = lb
	bs.ctx = ctx
	bs.pauseManager = service.FromContext[pause.Manager](ctx)

	if err = lb.Start(); err == nil {
		bs.isRunning = true
	}
	return err
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

// PauseVPN pauses the network for the specified duration. An error is returned if the network is
// already paused
func (bs *BoxService) PauseVPN(dur time.Duration) error {
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

// ServerConnectConfig represents configuration for connecting to a custom server.
type ServerConnectConfig []byte

// AddCustomServer load or parse the given configuration and add given
// enpdoint/outbound to the instance. We're only expecting one endpoint or
// outbound per call.
func (bs *BoxService) AddCustomServer(tag string, cfg ServerConnectConfig) error {
	bs.customServersMutex.Lock()
	defer bs.customServersMutex.Unlock()
	outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
	endpointManager := service.FromContext[adapter.EndpointManager](bs.ctx)
	router := service.FromContext[adapter.Router](bs.ctx)

	loadedOptions, configExist := bs.customServers[tag]
	if configExist {
		if err := bs.RemoveCustomServer(tag); err != nil {
			return err
		}
	}

	loadedOptions, err := json.UnmarshalExtendedContext[option.Options](bs.ctx, cfg)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	if len(loadedOptions.Endpoints) > 0 {
		for _, endpoint := range loadedOptions.Endpoints {
			endpointManager.Create(bs.ctx, router, bs.logFactory.NewLogger(fmt.Sprintf("custom_endpoints/%s", tag)), tag, endpoint.Type, endpoint.Options)
		}
	}

	if len(loadedOptions.Outbounds) > 0 {
		for _, outbound := range loadedOptions.Outbounds {
			outboundManager.Create(bs.ctx, router, bs.logFactory.NewLogger(fmt.Sprintf("custom_outbounds/%s", tag)), tag, outbound.Type, outbound.Options)
		}
	}

	// TODO: create a custom ruleset and fetch it from the new ruleset manager.

	// TODO: This function should persist the selected configured servers locally.
	// Since we're not storing configurations locally and don't have a directory
	// this info thill should be implemented in the future.
	bs.customServers[tag] = loadedOptions
	// TODO: update/refresh router
	return nil
}

func (bs *BoxService) RemoveCustomServer(tag string) error {
	bs.customServersMutex.Lock()
	defer bs.customServersMutex.Unlock()
	outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
	endpointManager := service.FromContext[adapter.EndpointManager](bs.ctx)

	if err := outboundManager.Remove(tag); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("failed to remove %q outbound: %w", tag, err)
	}

	if err := endpointManager.Remove(tag); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("failed to remove %q endpoint: %w", tag, err)
	}

	delete(bs.customServers, tag)
	return nil
}

// SelectCustomServer update the selector outbound to use the selected
// outbound based on provided tag. A selector outbound must exist before
// calling this function, otherwise it'll return a error.
func (bs *BoxService) SelectCustomServer(tag string) error {
	outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
	outbound, exist := outboundManager.Outbound("selector")
	if !exist {
		return fmt.Errorf("selector outbound not found")
	}
	selector, ok := outbound.(*group.Selector)
	if !ok {
		return fmt.Errorf("expected selector outbound to be a group.Selector")
	}
	selected := selector.SelectOutbound(tag)
	if !selected {
		return fmt.Errorf("failed to select custom server %q", tag)
	}
	return nil
}

func (bs *BoxService) Ctx() context.Context {
	return bs.ctx
}
