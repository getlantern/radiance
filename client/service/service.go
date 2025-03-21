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

	"github.com/getlantern/radiance/protocol"
)

// BoxService is a wrapper around libbox.BoxService
type BoxService struct {
	libbox *libbox.BoxService
	ctx    context.Context

	pauseManager pause.Manager
	pauseAccess  sync.Mutex
	pauseTimer   *time.Timer

	defaultOptions option.Options
	logFactory     log.Factory

	customServersMutex sync.Locker
	customServers      map[string]option.Options
}

// New creates a new BoxService that wraps a [libbox.BoxService]. platformInterface is used
// to interact with the underlying platform
func New(config, logOutput string, platIfce libbox.PlatformInterface) (*BoxService, error) {
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)
	lb, err := libbox.NewServiceWithContext(ctx, config, platIfce)
	if err != nil {
		return nil, fmt.Errorf("create libbox service: %w", err)
	}

	bs := &BoxService{
		libbox:             lb,
		ctx:                ctx,
		pauseManager:       service.FromContext[pause.Manager](ctx),
		pauseAccess:        sync.Mutex{},
		customServersMutex: new(sync.Mutex),
		customServers:      make(map[string]option.Options),
	}

	return bs, nil
}

func (bs *BoxService) NewLogger(name string) (log.Factory, error) {
	return log.New(log.Options{
		Context: bs.ctx,
		Options: *bs.defaultOptions.Log,
	})
}

func (bs *BoxService) Start() error {
	return bs.libbox.Start()
}

func (bs *BoxService) Close() error {
	return bs.libbox.Close()
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
	bs.pauseTimer = time.AfterFunc(dur, bs.pauseManager.NetworkWake)
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
	bs.pauseTimer.Stop()
	bs.pauseTimer = nil
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
		fmt.Errorf("failed to select custom server %q", tag)
	}
	return nil
}
