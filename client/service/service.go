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
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
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

	logFactory            log.Factory
	customServersMutex    sync.Locker
	customServers         map[string]option.Options
	customServersFilePath string
}

const CustomSelectorTag = "custom_selector"

type customServers struct {
	CustomServers []customServer `json:"custom_servers"`
}

type customServer struct {
	Tag     string         `json:"tag"`
	Options option.Options `json:"options"`
}

// New creates a new BoxService that wraps a [libbox.BoxService]. platformInterface is used
// to interact with the underlying platform
func New(config, dataDir string, platIfce libbox.PlatformInterface, rulesetManager *ruleset.Manager, logFactory log.Factory) (*BoxService, error) {
	bs := &BoxService{
		config:                config,
		platIfce:              platIfce,
		mutRuleSetManager:     rulesetManager,
		logFactory:            logFactory,
		customServersMutex:    new(sync.Mutex),
		customServers:         make(map[string]option.Options),
		customServersFilePath: filepath.Join(dataDir, "data", "custom_servers.json"),
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

// Start re-initialize the libbox service and start it. It will also start the ruleset manager
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

// Close stops the libbox service and clears the pause timer
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
	outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
	endpointManager := service.FromContext[adapter.EndpointManager](bs.ctx)
	router := service.FromContext[adapter.Router](bs.ctx)

	bs.customServersMutex.Lock()
	loadedOptions, configExist := bs.customServers[tag]
	if configExist {
		bs.customServersMutex.Unlock()
		if err := bs.RemoveCustomServer(tag); err != nil {
			return err
		}
	}
	bs.customServersMutex.Unlock()

	loadedOptions, err := json.UnmarshalExtendedContext[option.Options](bs.ctx, cfg)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	if len(loadedOptions.Endpoints) > 0 {
		for _, endpoint := range loadedOptions.Endpoints {
			err = endpointManager.Create(bs.ctx, router, bs.logFactory.NewLogger(fmt.Sprintf("custom/%s/%s", tag, endpoint.Tag)), endpoint.Tag, endpoint.Type, endpoint.Options)
			if err != nil {
				return fmt.Errorf("failed to create endpoint %q: %w", endpoint.Tag, err)
			}
		}
	}

	if len(loadedOptions.Outbounds) > 0 {
		for _, outbound := range loadedOptions.Outbounds {
			err = outboundManager.Create(bs.ctx, router, bs.logFactory.NewLogger(fmt.Sprintf("custom/%s/%s", tag, outbound.Tag)), outbound.Tag, outbound.Type, outbound.Options)
			if err != nil {
				return fmt.Errorf("failed to create outbound %q: %w", outbound.Tag, err)
			}
		}
	}

	bs.customServersMutex.Lock()
	defer bs.customServersMutex.Unlock()
	bs.customServers[tag] = loadedOptions
	if err = bs.storeCustomServer(tag, loadedOptions); err != nil {
		return fmt.Errorf("store custom server: %w", err)
	}

	return nil
}

// storeCustomServer stores the custom server configuration to a JSON file.
func (bs *BoxService) storeCustomServer(tag string, options option.Options) error {
	servers, err := bs.loadCustomServer()
	if err != nil {
		return fmt.Errorf("load custom servers: %w", err)
	}

	if len(servers.CustomServers) == 0 {
		servers.CustomServers = make([]customServer, 0)
		servers.CustomServers = append(servers.CustomServers, customServer{
			Tag:     tag,
			Options: options,
		})
	} else {
		for i, server := range servers.CustomServers {
			if server.Tag == tag {
				server.Options = options
				servers.CustomServers[i] = server
				break
			}
		}
	}

	if err = bs.writeChanges(servers); err != nil {
		return fmt.Errorf("failed to add custom server %q: %w", tag, err)
	}

	return nil
}

func (bs *BoxService) writeChanges(customServers customServers) error {
	storedCustomServers, err := json.MarshalContext(bs.ctx, customServers)
	if err != nil {
		return fmt.Errorf("marshal custom servers: %w", err)
	}
	if err := os.WriteFile(bs.customServersFilePath, storedCustomServers, 0644); err != nil {
		return fmt.Errorf("write custom servers file: %w", err)
	}
	return nil
}

// loadCustomServer loads the custom server configuration from a JSON file.
func (bs *BoxService) loadCustomServer() (customServers, error) {
	var cs customServers
	// read file and generate []byte
	storedCustomServers, err := os.ReadFile(bs.customServersFilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// file not exist, return empty custom servers
			return cs, nil
		}
		return cs, fmt.Errorf("read custom servers file: %w", err)
	}

	if err := json.UnmarshalContext(bs.ctx, storedCustomServers, &cs); err != nil {
		return cs, fmt.Errorf("decode custom servers file: %w", err)
	}

	return cs, nil
}

func (bs *BoxService) removeCustomServer(tag string) error {
	customServers, err := bs.loadCustomServer()
	if err != nil {
		return fmt.Errorf("load custom servers: %w", err)
	}
	for i, server := range customServers.CustomServers {
		if server.Tag == tag {
			customServers.CustomServers = append(customServers.CustomServers[:i], customServers.CustomServers[i+1:]...)
			break
		}
	}
	if err = bs.writeChanges(customServers); err != nil {
		return fmt.Errorf("failed to write custom server %q removal: %w", tag, err)
	}
	return nil
}

// RemoveCustomServer removes the custom server options from endpoints, outbounds
// and the custom server file.
func (bs *BoxService) RemoveCustomServer(tag string) error {
	bs.customServersMutex.Lock()
	defer bs.customServersMutex.Unlock()
	outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
	endpointManager := service.FromContext[adapter.EndpointManager](bs.ctx)

	options := bs.customServers[tag]
	for _, outbounds := range options.Outbounds {
		if err := outboundManager.Remove(outbounds.Tag); err != nil && !errors.Is(err, os.ErrInvalid) {
			return fmt.Errorf("failed to remove %q outbound: %w", tag, err)
		}
	}

	for _, endpoints := range options.Endpoints {
		if err := endpointManager.Remove(endpoints.Tag); err != nil && !errors.Is(err, os.ErrInvalid) {
			return fmt.Errorf("failed to remove %q endpoint: %w", tag, err)
		}
	}

	delete(bs.customServers, tag)
	if err := bs.removeCustomServer(tag); err != nil {
		return fmt.Errorf("failed to remove custom server %q: %w", tag, err)
	}
	return nil
}

// SelectCustomServer update the selector outbound to use the selected
// outbound based on provided tag. A selector outbound must exist before
// calling this function, otherwise it'll return a error.
func (bs *BoxService) SelectCustomServer(tag string) error {
	outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
	outbounds := outboundManager.Outbounds()
	tags := make([]string, len(outbounds)-1)
	for i, outbound := range outbounds {
		// ignoring selector because it'll be removed and re-added with the new tags
		if outbound.Tag() == CustomSelectorTag {
			continue
		}
		tags[i] = outbound.Tag()
	}

	// removing custom selector for re-adding with new fresh outbound tags
	if err := outboundManager.Remove(CustomSelectorTag); err != nil {
		return fmt.Errorf("failed to remove selector outbound: %w", err)
	}

	err := bs.newSelectorOutbound(outboundManager, CustomSelectorTag, option.SelectorOutboundOptions{
		Outbounds:                 tags,
		Default:                   tag,
		InterruptExistConnections: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create selector outbound: %w", err)
	}

	return nil
}

func (bs *BoxService) newSelectorOutbound(outboundManager adapter.OutboundManager, tag string, options option.SelectorOutboundOptions) error {
	router := service.FromContext[adapter.Router](bs.ctx)

	if err := outboundManager.Create(bs.ctx, router, bs.logFactory.NewLogger(tag), tag, constant.TypeSelector, options); err != nil {
		return fmt.Errorf("create selector outbound: %w", err)
	}

	return nil
}

func (bs *BoxService) Ctx() context.Context {
	return bs.ctx
}
