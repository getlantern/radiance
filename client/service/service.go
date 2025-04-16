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
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"github.com/getlantern/sing-box-extensions/ruleset"

	"github.com/getlantern/radiance/config"
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
	CustomServers []CustomServerInfo `json:"custom_servers"`
}

type CustomServerInfo struct {
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
		return fmt.Errorf("create libbox service: %w", err)
	}
	service.MustRegister(ctx, lb.LogFactory())

	// we need to start the ruleset manager before starting the libbox service but after the libbox
	// service has been initialized so that the ruleset manager can access the routing rules.
	if err := bs.mutRuleSetManager.Start(ctx); err != nil {
		return fmt.Errorf("start ruleset manager: %w", err)
	}

	ctx = service.ContextWithPtr(ctx, bs.mutRuleSetManager)
	bs.libbox = lb
	bs.ctx = ctx
	bs.pauseManager = service.FromContext[pause.Manager](ctx)

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

// ServerConnectConfig represents configuration for connecting to a custom server.
type ServerConnectConfig []byte

// AddCustomServer load or parse the given configuration and add given
// endpdoint/outbound to the instance. We're only expecting one endpoint or
// outbound per call.
func (bs *BoxService) AddCustomServer(tag string, cfg ServerConnectConfig) error {
	bs.customServersMutex.Lock()
	loadedOptions, configExist := bs.customServers[tag]
	bs.customServersMutex.Unlock()
	if configExist && cfg != nil {
		if err := bs.RemoveCustomServer(tag); err != nil {
			return err
		}
	}

	if cfg != nil {
		var err error
		loadedOptions, err = json.UnmarshalExtendedContext[option.Options](bs.ctx, cfg)
		if err != nil {
			return fmt.Errorf("failed to unmarshal config: %w", err)
		}
	}

	if err := updateOutboundsEndpoints(bs.ctx, loadedOptions.Outbounds, loadedOptions.Endpoints); err != nil {
		return fmt.Errorf("failed to update outbounds/endpoints: %w", err)
	}

	bs.customServersMutex.Lock()
	bs.customServers[tag] = loadedOptions
	bs.customServersMutex.Unlock()
	if err := bs.storeCustomServer(tag, loadedOptions); err != nil {
		return fmt.Errorf("failed to store custom server: %w", err)
	}

	return nil
}

func (bs *BoxService) ListCustomServers() ([]CustomServerInfo, error) {
	loadedServers, err := bs.loadCustomServer()
	if err != nil {
		return nil, fmt.Errorf("failed to load custom servers: %w", err)
	}

	return loadedServers.CustomServers, nil
}

// storeCustomServer stores the custom server configuration to a JSON file.
func (bs *BoxService) storeCustomServer(tag string, options option.Options) error {
	servers, err := bs.loadCustomServer()
	if err != nil {
		return fmt.Errorf("load custom servers: %w", err)
	}

	if len(servers.CustomServers) == 0 {
		servers.CustomServers = make([]CustomServerInfo, 0)
		servers.CustomServers = append(servers.CustomServers, CustomServerInfo{
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

	bs.customServersMutex.Lock()
	defer bs.customServersMutex.Unlock()
	for _, v := range cs.CustomServers {
		bs.customServers[v.Tag] = v.Options
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
	outboundManager := service.FromContext[adapter.OutboundManager](bs.ctx)
	endpointManager := service.FromContext[adapter.EndpointManager](bs.ctx)

	bs.customServersMutex.Lock()
	options := bs.customServers[tag]
	bs.customServersMutex.Unlock()
	// selector must be removed in order to remove dependent outbounds
	if err := outboundManager.Remove(CustomSelectorTag); err != nil {
		return fmt.Errorf("failed to remove selector outbound: %w", err)
	}
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

	bs.customServersMutex.Lock()
	delete(bs.customServers, tag)
	bs.customServersMutex.Unlock()
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
	tags := make([]string, 0)
	for _, outbound := range outbounds {
		// ignoring selector because it'll be removed and re-added with the new tags
		if outbound.Tag() == CustomSelectorTag {
			continue
		}
		tags = append(tags, outbound.Tag())
	}

	if _, exists := outboundManager.Outbound(CustomSelectorTag); exists {
		if err := outboundManager.Remove(CustomSelectorTag); err != nil {
			return fmt.Errorf("failed to remove selector outbound: %w", err)
		}
	}

	err := bs.newSelectorOutbound(outboundManager, CustomSelectorTag, &option.SelectorOutboundOptions{
		Outbounds:                 tags,
		Default:                   tag,
		InterruptExistConnections: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create selector outbound: %w", err)
	}

	outbound, ok := outboundManager.Outbound(CustomSelectorTag)
	if !ok {
		return fmt.Errorf("failed to get selector outbound: %w", err)
	}
	selector, ok := outbound.(*group.Selector)
	if !ok {
		return fmt.Errorf("expected outbound of type *group.Selector: %w", err)
	}
	if err = selector.Start(); err != nil {
		return fmt.Errorf("failed to start selector outbound: %w", err)
	}
	if ok = selector.SelectOutbound(tag); !ok {
		return fmt.Errorf("failed to select outbound %q: %w", tag, err)
	}

	return nil
}

func (bs *BoxService) newSelectorOutbound(outboundManager adapter.OutboundManager, tag string, options *option.SelectorOutboundOptions) error {
	router := service.FromContext[adapter.Router](bs.ctx)
	logFactory := service.FromContext[log.Factory](bs.ctx)
	if err := outboundManager.Create(bs.ctx, router, logFactory.NewLogger(tag), tag, constant.TypeSelector, options); err != nil {
		return fmt.Errorf("create selector outbound: %w", err)
	}

	return nil
}

func (bs *BoxService) Ctx() context.Context {
	return bs.ctx
}

// OnNewConfig is called when a new configuration is received. It updates the VPN client with the
// new configuration and restarts the VPN client if necessary.
func (bs *BoxService) OnNewConfig(_, newConfig *config.Config) error {
	slog.Debug("Received new config")

	return updateOutboundsEndpoints(bs.ctx, newConfig.ConfigResponse.Options.Outbounds,
		newConfig.ConfigResponse.Options.Endpoints)
}

func (bs *BoxService) ParseConfig(configRaw []byte) (*config.Config, error) {
	config, err := json.UnmarshalExtendedContext[*config.Config](bs.ctx, configRaw)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return config, nil
}

var (
	permanentOutbounds = []string{
		"direct",
		"dns",
		"block",
		CustomSelectorTag,
	}
	permanentEndpoints = []string{}
)

// updateOutboundsEndpoints updates the outbounds and endpoints in the router, skipping any present in
// [permanentOutbounds] and [permanentEndpoints]. updateOutboundsEndpoints will continue processing
// the remaining outbounds and endpoints even if an error occurs, returning a single error of all
// errors encountered.
func updateOutboundsEndpoints(ctx context.Context, outbounds []option.Outbound, endpoints []option.Endpoint) error {
	router := service.FromContext[adapter.Router](ctx)
	if router == nil {
		return errors.New("router missing from context")
	}

	logFactory := service.FromContext[log.Factory](ctx)
	var errs error
	if len(outbounds) > 0 {
		outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
		if outboundMgr == nil {
			errs = errors.Join(errs, errors.New("outbound manager missing from context"))
		} else {
			err := updateOutbounds(ctx, outboundMgr, router, logFactory, outbounds, permanentOutbounds)
			if err != nil {
				errs = fmt.Errorf("update outbounds: %w", err)
			}
		}
	}
	if len(endpoints) > 0 {
		endpointMgr := service.FromContext[adapter.EndpointManager](ctx)
		if endpointMgr == nil {
			errs = errors.Join(errs, errors.New("endpoint manager missing from context"))
		} else {
			err := updateEndpoints(ctx, endpointMgr, router, logFactory, endpoints, permanentEndpoints)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("update endpoints: %w", err))
			}
		}
	}
	return errs
}

// updateOutbounds syncs the [adapter.OutboundManager] with the provided outbounds. It skips excluded
// or untagged entries, removes outdated ones, and creates or updates the rest. If any error occurs,
// updateOutbounds will continue processing the remaining outbounds and return a single error of
// all errors encountered.
func updateOutbounds(
	ctx context.Context,
	outboundMgr adapter.OutboundManager,
	router adapter.Router,
	logFactory log.Factory,
	outbounds []option.Outbound,
	excludeTags []string,
) error {
	newItems, errs := filterItems(outbounds, excludeTags, func(it option.Outbound) string {
		return it.Tag
	})

	errs = errors.Join(errs, removeItems(
		outboundMgr.Outbounds(),
		newItems,
		excludeTags,
		outboundMgr.Remove,
	))

	for tag, outbound := range newItems {
		logger := logFactory.NewLogger(fmt.Sprintf("outbound/%s[%s]", outbound.Type, tag))
		err := outboundMgr.Create(ctx, router, logger, tag, outbound.Type, outbound.Options)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("initialize [%v]: %w", tag, err))
		}
	}

	return errs
}

// updateEndpoints syncs the [adapter.EndpointManager] with the provided [option.Endpoint]s. It skips
// excluded or untagged entries, removes outdated ones, and creates or updates the rest. If any error
// occurs, updateEndpoints will continue processing the remaining endpoints and return a single error
// of all errors encountered.
func updateEndpoints(
	ctx context.Context,
	endpointMgr adapter.EndpointManager,
	router adapter.Router,
	logFactory log.Factory,
	endpoints []option.Endpoint,
	excludeTags []string,
) error {
	// filter endpoints that are missing a tag or are excluded
	newItems, errs := filterItems(endpoints, excludeTags, func(it option.Endpoint) string {
		return it.Tag
	})

	if nToRemove := len(endpointMgr.Endpoints()) - len(newItems); nToRemove > 0 {
		errs = errors.Join(errs, removeItems(
			endpointMgr.Endpoints(),
			newItems,
			excludeTags,
			endpointMgr.Remove,
		))
	}

	for tag, endpoint := range newItems {
		logger := logFactory.NewLogger(fmt.Sprintf("endpoint/%s[%s]", endpoint.Type, tag))
		err := endpointMgr.Create(ctx, router, logger, tag, endpoint.Type, endpoint.Options)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("initialize [%v]: %w", tag, err))
		}
	}

	return errs
}

// filterItems returns a map of items with tags as keys. It filters out items that are missing a tag
// or are present in excludeTags. An error is returned listing all items that are missing a tag.
func filterItems[T any](items []T, excludeTags []string, getTag func(T) string) (map[string]T, error) {
	var errs error
	filtered := make(map[string]T)
	for idx, it := range items {
		switch tag := getTag(it); {
		case tag == "":
			errs = errors.Join(errs, fmt.Errorf("missing tag for %T[%d]", it, idx))
		case slices.Contains(excludeTags, tag):
		default:
			filtered[tag] = it
		}
	}
	return filtered, errs
}

// bA is a one-off interface to allow for generic handling of both [adapter.Outbound] and
// [adapter.Endpoint] types.
type bA interface {
	Tag() string
	Type() string
}

// removeItems removes items not present in newItems or excludeTags using the provided remove function.
// If an error occurs, it continues processing the remaining items and returns a single error of all
// errors encountered.
func removeItems[I ~[]T, T bA, O any](
	items I,
	newItems map[string]O,
	excludeTags []string,
	remove func(string) error,
) error {
	var errs error
	for i := 0; i < len(items); {
		it := items[i]
		if bA(it) == bA(nil) {
			break
		}
		tag := it.Tag()
		if _, ok := newItems[tag]; !ok && !slices.Contains(excludeTags, tag) {
			if err := remove(tag); err != nil {
				errs = errors.Join(errs, fmt.Errorf("remove [%v]: %w", tag, err))
			}
		} else {
			i++
		}
	}
	return errs
}
