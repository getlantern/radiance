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
	"slices"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"github.com/getlantern/sing-box-extensions/protocol"
	"github.com/getlantern/sing-box-extensions/ruleset"

	"github.com/getlantern/radiance/config"
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

const CustomSelectorTag = "custom_selector"

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
