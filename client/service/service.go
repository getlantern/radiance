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
	"sync/atomic"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"github.com/getlantern/sing-box-extensions/protocol"
	"github.com/getlantern/sing-box-extensions/ruleset"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/client/boxoptions"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
)

var (
	baseCtx   context.Context
	ctxAccess sync.Mutex
)

// BoxService is a wrapper around libbox.BoxService
type BoxService struct {
	libbox            *libbox.BoxService
	ctx               context.Context
	platIfce          libbox.PlatformInterface
	mutRuleSetManager *ruleset.Manager

	pauseManager pause.Manager
	pauseAccess  sync.Mutex
	pauseTimer   *time.Timer

	options         option.Options
	optionsAccess   sync.Mutex
	configPath      string
	optsFileWatcher *internal.FileWatcher

	userServerManager *CustomServerManager
	clashServer       *clashapi.Server

	activeServer atomic.Value

	mu        sync.Mutex
	isRunning bool
}

// New creates a new BoxService that wraps a [libbox.BoxService]. platformInterface is used
// to interact with the underlying platform
func New(
	options, baseDir, configFilename string,
	platIfce libbox.PlatformInterface,
	rulesetManager *ruleset.Manager,
	userServerManager *CustomServerManager,
) (*BoxService, error) {
	slog.Info("Creating boxservice", slog.String("options", options))
	opts, err := json.UnmarshalExtendedContext[option.Options](BaseContext(), []byte(options))
	if err != nil {
		return nil, fmt.Errorf("unmarshal options: %w", err)
	}

	bs := &BoxService{
		ctx:               newBaseContext(),
		options:           opts,
		platIfce:          platIfce,
		mutRuleSetManager: rulesetManager,
		configPath:        filepath.Join(baseDir, configFilename),
		userServerManager: userServerManager,
	}
	bs.activeServer.Store(&Server{})

	// create the config file watcher to reload the options when the config file changes
	watcher := internal.NewFileWatcher(bs.configPath, func() {
		err := bs.reloadOptions()
		if err != nil {
			slog.Error("Failed to reload options", "error", err)
		}
		slog.Debug("Reloaded options")
	})
	if err := watcher.Start(); err != nil {
		return nil, fmt.Errorf("start config file watcher: %w", err)
	}
	bs.optsFileWatcher = watcher

	setupOpts := &libbox.SetupOptions{
		BasePath:    baseDir,
		WorkingPath: baseDir,
		TempPath:    filepath.Join(baseDir, "temp"),
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
	bs.optionsAccess.Lock()
	options := bs.options
	bs.optionsAccess.Unlock()

	insertUserServers(options, bs.userServerManager.ListCustomServers())
	if err := setInitialServer(options, bs.activeServer.Load().(*Server)); err != nil {
		return fmt.Errorf("failed to select server: %w", err)
	}

	lb, ctx, err := newLibboxService(options, bs.platIfce)
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
	bs.clashServer = service.FromContext[adapter.ClashServer](ctx).(*clashapi.Server)

	if err := bs.libbox.Start(); err != nil {
		return fmt.Errorf("error starting libbox service: %w", err)
	}
	bs.isRunning = true
	return nil
}

func setInitialServer(opts option.Options, server *Server) error {
	if server.Group != boxoptions.ServerGroupUser && server.Group != boxoptions.ServerGroupLantern {
		return fmt.Errorf("invalid server group: %s", server.Group)
	}
	group := server.Group
	idx := slices.IndexFunc(opts.Outbounds, func(o option.Outbound) bool {
		return o.Tag == group && o.Type == constant.TypeSelector
	})
	if idx < 0 {
		return fmt.Errorf("no selector outbound found for group %s", group)
	}
	out := opts.Outbounds[idx]
	sOpts := out.Options.(*option.SelectorOutboundOptions)
	sOpts.Default = server.Name
	opts.Experimental.ClashAPI.DefaultMode = group
	return nil
}

func insertUserServers(opts option.Options, servers []CustomServerInfo) {
	if len(servers) == 0 {
		return
	}
	tags := make([]string, 0, len(servers))
	for _, server := range servers {
		// insert server outbounds/endpoints into the options if they are not already present
		if server.Outbound != nil {
			if !slices.ContainsFunc(opts.Outbounds, func(o option.Outbound) bool {
				return o.Tag == server.Outbound.Tag
			}) {
				opts.Outbounds = append(opts.Outbounds, *server.Outbound)
				tags = append(tags, server.Outbound.Tag)
			}
		} else if server.Endpoint != nil {
			if !slices.ContainsFunc(opts.Endpoints, func(e option.Endpoint) bool {
				return e.Tag == server.Endpoint.Tag
			}) {
				opts.Endpoints = append(opts.Endpoints, *server.Endpoint)
				tags = append(tags, server.Endpoint.Tag)
			}
		}
	}

	idx := slices.IndexFunc(opts.Outbounds, func(o option.Outbound) bool {
		return o.Tag == boxoptions.ServerGroupUser && o.Type == constant.TypeSelector
	})
	selector := boxoptions.SelectorOutbound(tags, boxoptions.ServerGroupUser, "direct")
	if idx >= 0 {
		opts.Outbounds[idx] = selector
	} else {
		opts.Outbounds = append(opts.Outbounds, selector)
	}
	return
}

func BaseContext() context.Context {
	ctxAccess.Lock()
	defer ctxAccess.Unlock()
	if baseCtx == nil {
		baseCtx = newBaseContext()
	}
	return baseCtx
}

func newBaseContext() context.Context {
	// Retrieve protocol registries
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	return box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)
}

// newLibboxService creates a new libbox service with the given config and platform interface
func newLibboxService(opts option.Options, platIfce libbox.PlatformInterface) (*libbox.BoxService, context.Context, error) {
	// initialize the libbox service
	// we need to create a new context each time so we have a fresh context, free of all the values
	// that the sing-box instance adds to it
	ctx := newBaseContext()

	conf, err := json.MarshalContext(ctx, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal options: %w", err)
	}
	slog.Debug("Creating libbox service", slog.String("options", string(conf)))
	lb, err := libbox.NewServiceWithContext(ctx, string(conf), platIfce)
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

func (bs *BoxService) SelectServer(group, tag string) error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	// TODO: handle the case where the group is ServerGroupLantern
	if group == boxoptions.ServerGroupLantern {
		return errors.New("lantern group is not supported for selecting servers yet")
	}

	if group != boxoptions.ServerGroupUser && group != boxoptions.ServerGroupLantern {
		return fmt.Errorf("invalid group: %s, must be %s or %s", group, boxoptions.ServerGroupUser, boxoptions.ServerGroupLantern)
	}
	if tag == "" {
		return errors.New("tag must be specified")
	}
	if bs.userServerManager == nil {
		return errors.New("user server manager is not initialized")
	}

	server, fnd := bs.userServerManager.GetServerByTag(tag)
	if !fnd {
		return fmt.Errorf("server with tag %s not found", tag)
	}
	var selectedServer Server
	switch server := server.(type) {
	case *option.Outbound:
		config, err := json.MarshalContext(bs.ctx, server.Options)
		if err != nil {
			return fmt.Errorf("marshal outbound options: %w", err)
		}
		selectedServer = Server{
			Name:     server.Tag,
			Config:   string(config),
			Protocol: server.Type,
			Group:    group,
		}
	case *option.Endpoint:
		config, err := json.MarshalContext(bs.ctx, server.Options)
		if err != nil {
			return fmt.Errorf("marshal endpoint options: %w", err)
		}
		selectedServer = Server{
			Name:     server.Tag,
			Config:   string(config),
			Protocol: server.Type,
			Group:    group,
		}
	default:
		return fmt.Errorf("unsupported server type: %T", server)
	}
	if !bs.isRunning {
		bs.activeServer.Store(&selectedServer)
		return nil
	}

	if err := libbox.NewStandaloneCommandClient().SelectOutbound(group, tag); err != nil {
		return fmt.Errorf("select server: %w", err)
	}
	bs.activeServer.Store(&selectedServer)
	bs.clashServer.SetMode(group)
	return nil
}

type Server struct {
	Name     string // config.Tag
	Location C.ServerLocation
	Config   string // option.Outbound or option.Endpoint
	Protocol string // config.Type
	Group    string // lantern or user
}

// TODO: need to retrieve which outbound is currently being used..

// ActiveServer returns the currently active server.
// Not Implemented
func (bs *BoxService) ActiveServer() (Server, error) {
	return Server{}, common.ErrNotImplemented
}

// reloadOptions reloads the options from the config file. If boxservice is running, the outbounds
// and endpoints are updated in the router.
func (bs *BoxService) reloadOptions() error {
	// TODO:
	//		- restart libbox if options change that can't be updated while running (routing, etc)
	//		- update all options. make sure to include base/default options where needed (DNS)
	slog.Debug("reloading options")
	content, err := os.ReadFile(bs.configPath)
	if os.IsNotExist(err) {
		slog.Debug("config file not found, skipping reload")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	conf, err := UnmarshalConfig(content)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	opts := conf.Options
	bs.optionsAccess.Lock()
	currOpts := bs.options

	currOpts.Outbounds = append(boxoptions.BaseOutbounds, opts.Outbounds...)
	currOpts.Endpoints = append(boxoptions.BaseEndpoints, opts.Endpoints...)

	// add custom server outbounds/endpoints
	csm := service.PtrFromContext[CustomServerManager](bs.ctx)
	if csm != nil {
		servers := csm.ListCustomServers()
		for _, server := range servers {
			if server.Outbound != nil {
				currOpts.Outbounds = append(currOpts.Outbounds, *server.Outbound)
			} else if server.Endpoint != nil {
				currOpts.Endpoints = append(currOpts.Endpoints, *server.Endpoint)
			}
		}
	}

	bs.options = currOpts
	bs.optionsAccess.Unlock()

	bs.mu.Lock()
	if !bs.isRunning {
		bs.mu.Unlock()
		return nil
	}
	bs.mu.Unlock()

	slog.Debug("updating outbounds/endpoints")
	err = updateOutboundsEndpoints(bs.ctx, opts.Outbounds, opts.Endpoints)
	if err != nil {
		return fmt.Errorf("update outbounds/endpoints: %w", err)
	}

	return nil
}

func UnmarshalConfig(configRaw []byte) (*C.ConfigResponse, error) {
	config, err := json.UnmarshalExtendedContext[C.ConfigResponse](BaseContext(), configRaw)
	if err != nil {
		return nil, fmt.Errorf("unmarshal options: %w", err)
	}

	return &config, nil
}

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
	permOut, permEP := boxoptions.PermanentOutboundsEndpoints()
	var errs error
	if len(outbounds) > 0 {
		outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
		if outboundMgr == nil {
			errs = errors.Join(errs, errors.New("outbound manager missing from context"))
		} else {
			err := updateOutbounds(ctx, outboundMgr, router, logFactory, outbounds, permOut)
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
			err := updateEndpoints(ctx, endpointMgr, router, logFactory, endpoints, permEP)
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
	slog.Debug("Updating outbounds", slog.Any("outbounds", outbounds))
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
		logger.Debug("Creating outbound", tag, "[", outbound.Type, "]")
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
	slog.Debug("Updating endpoints", slog.Any("endpoints", endpoints))
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
		logger.Debug("Creating endpoint", tag, "[", endpoint.Type, "]")
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
