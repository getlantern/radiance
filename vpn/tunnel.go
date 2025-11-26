package vpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	lcommon "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/adapter/groups"
	lblog "github.com/getlantern/lantern-box/log"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn/ipc"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
	sblog "github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

var (
	tInstance *tunnel
	tAccess   sync.Mutex

	ipcServer *ipc.Server
)

type tunnel struct {
	ctx         context.Context
	lbService   *libbox.BoxService
	cacheFile   adapter.CacheFile
	clashServer *clashapi.Server
	logFactory  sblog.ObservableFactory

	svrFileWatcher *internal.FileWatcher
	reloadAccess   sync.Mutex
	// optsMap is a map of current outbound/endpoint options JSON, used to deduplicate on reload
	optsMap   map[string][]byte
	mutGrpMgr *groups.MutableGroupManager

	status  atomic.Value
	cancel  context.CancelFunc
	closers []io.Closer
}

// establishConnection initializes and starts the VPN tunnel with the provided options and platform interface.
func establishConnection(group, tag string, opts O.Options, dataPath string, platIfce libbox.PlatformInterface) (err error) {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance != nil {
		slog.Warn("Tunnel already opened", "group", group, "tag", tag)
		return errors.New("tunnel already opened")
	}

	slog.Info("Establishing VPN tunnel", "group", group, "tag", tag)

	t := &tunnel{}
	t.status.Store(ipc.StatusInitializing)

	t.ctx, t.cancel = context.WithCancel(box.BoxContext())
	if err := t.init(opts, dataPath, platIfce); err != nil {
		slog.Error("Failed to initialize tunnel", "error", err)
		return fmt.Errorf("initializing tunnel: %w", err)
	}

	// we need to set the selected server before starting libbox
	slog.Log(nil, internal.LevelTrace, "Starting cachefile")
	if err := t.cacheFile.Start(adapter.StartStateInitialize); err != nil {
		slog.Error("Failed to start cache file", "error", err)
		return fmt.Errorf("start cache file: %w", err)
	}
	t.closers = append(t.closers, t.cacheFile)

	if group == "" { // group is empty, connect to last selected server
		slog.Debug("Connecting to last selected server")
		err = t.connect()
	} else {
		err = t.connectTo(group, tag)
	}
	if err != nil {
		slog.Error("Failed to connect tunnel", "error", err)
		t.close()
		return err
	}
	t.optsMap = makeOutboundOptsMap(t.ctx, opts.Outbounds, opts.Endpoints)

	tInstance = t
	t.status.Store(ipc.StatusRunning)
	// If the IPC server is already running, make sure it points to the live tunnel
	if ipcServer != nil {
		ipcServer.SetService(t)
		return nil
	}
	// fallback: start IPC server here for platforms that don't call InitIPC yet
	isvr := ipc.NewServer(t)
	if err := isvr.Start(dataPath); err != nil {
		slog.Error("Failed to start IPC server", "error", err)
		t.close()
		return fmt.Errorf("starting IPC server: %w", err)
	}
	ipcServer = isvr
	slog.Debug("IPC server started")
	return nil
}

func (t *tunnel) init(opts O.Options, dataPath string, platIfce libbox.PlatformInterface) error {
	slog.Log(nil, internal.LevelTrace, "Initializing tunnel")

	cfg, err := json.MarshalContext(t.ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to marshal options: %w", err)
	}

	// setup libbox service
	setupOpts := &libbox.SetupOptions{
		BasePath:    dataPath,
		WorkingPath: dataPath,
		TempPath:    filepath.Join(dataPath, "temp"),
	}
	if common.Platform == "android" {
		setupOpts.FixAndroidStack = true
	}
	slog.Log(nil, internal.LevelTrace, "Setting up libbox", "setup_options", setupOpts)

	if err := libbox.Setup(setupOpts); err != nil {
		return fmt.Errorf("setup libbox: %w", err)
	}

	t.logFactory = lblog.NewFactory(slog.Default().Handler())
	service.MustRegister[sblog.Factory](t.ctx, t.logFactory)

	// create the cache file service
	if opts.Experimental.CacheFile == nil {
		return fmt.Errorf("cache file options are required")
	}
	cacheFile := cachefile.New(t.ctx, *opts.Experimental.CacheFile)
	service.MustRegister[adapter.CacheFile](t.ctx, cacheFile)
	t.cacheFile = cacheFile

	slog.Log(nil, internal.LevelTrace, "Creating libbox service")
	lb, err := libbox.NewServiceWithContext(t.ctx, string(cfg), platIfce)
	if err != nil {
		return fmt.Errorf("create libbox service: %w", err)
	}
	t.lbService = lb

	// set memory limit for Android and iOS
	switch common.Platform {
	case "android", "ios":
		slog.Debug("Setting memory limit for mobile platform", "platform", common.Platform)
		libbox.SetMemoryLimit(true)
	default:
	}

	// create file watcher for server changes
	svrsPath := filepath.Join(dataPath, common.ServersFileName)
	svrWatcher := internal.NewFileWatcher(svrsPath, func() {
		slog.Debug("Server file change detected", "path", svrsPath)
		err := t.reloadOptions(svrsPath)
		switch {
		case errors.Is(err, context.Canceled):
			slog.Debug("Tunnel is closing, ignoring server reload")
		case err != nil:
			slog.Error("Failed to reload servers", "error", err)
		default:
			slog.Debug("Servers reloaded successfully")
		}
	})
	t.svrFileWatcher = svrWatcher
	slog.Info("Tunnel initializated")
	return nil
}

func newMutableGroupManager(
	ctx context.Context,
	logger sblog.ContextLogger,
	connMgr groups.ConnectionManager,
) (*groups.MutableGroupManager, error) {
	oMgr := service.FromContext[adapter.OutboundManager](ctx)
	epMgr := service.FromContext[adapter.EndpointManager](ctx)
	if oMgr == nil || epMgr == nil {
		return nil, fmt.Errorf("outbound or endpoint manager not found in context")
	}

	var mutGroups []lbA.MutableOutboundGroup
	for _, out := range oMgr.Outbounds() {
		if g, isMutGroup := out.(lbA.MutableOutboundGroup); isMutGroup {
			mutGroups = append(mutGroups, g)
		}
	}
	return groups.NewMutableGroupManager(logger, oMgr, epMgr, connMgr, mutGroups), nil
}

func (t *tunnel) connect() (err error) {
	slog.Log(nil, internal.LevelTrace, "Starting libbox service")

	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic starting libbox service", "panic", r)
			err = fmt.Errorf("panic starting libbox service: %v", r)
		}
	}()
	if err := t.lbService.Start(); err != nil {
		slog.Error("Failed to start libbox service", "error", err)
		return fmt.Errorf("starting libbox service: %w", err)
	}
	slog.Debug("Libbox service started")
	t.closers = append(t.closers, t.lbService)

	t.clashServer = service.FromContext[adapter.ClashServer](t.ctx).(*clashapi.Server)

	mutGrpMgr, err := newMutableGroupManager(
		t.ctx, t.logFactory.NewLogger("groupsManager"), t.clashServer.TrafficManager(),
	)
	if err != nil {
		return fmt.Errorf("creating mutable group manager: %w", err)
	}
	t.mutGrpMgr = mutGrpMgr

	if err := t.svrFileWatcher.Start(); err != nil {
		slog.Error("Failed to start user server file watcher", "error", err)
		return fmt.Errorf("starting user server file watcher: %w", err)
	}
	t.closers = append(t.closers, t.svrFileWatcher)

	slog.Info("Tunnel connection established")
	return nil
}

func (t *tunnel) connectTo(group, tag string) error {
	err := t.cacheFile.StoreMode(group)
	if err == nil {
		err = t.cacheFile.StoreSelected(group, tag)
	}
	if err != nil {
		slog.Error("failed to set selected server", "group", group, "tag", tag, "error", err)
		return fmt.Errorf("set selected server %s.%s: %w", group, tag, err)
	}
	slog.Debug("set selected server", "group", group, "tag", tag)
	return t.connect()
}

func (t *tunnel) Close() error {
	if t.status.Swap(ipc.StatusClosed) == ipc.StatusClosed {
		return nil
	}
	tAccess.Lock()
	defer tAccess.Unlock()
	tInstance = nil
	return t.close()
}

func (t *tunnel) close() error {
	slog.Info("Closing tunnel")
	t.cancel()

	done := make(chan error)
	go func() {
		var errs []error
		for _, closer := range t.closers {
			slog.Log(nil, internal.LevelTrace, "Closing tunnel resource", "type", fmt.Sprintf("%T", closer))
			errs = append(errs, closer.Close())
		}
		done <- errors.Join(errs...)
	}()
	var err error
	select {
	case <-time.After(10 * time.Second):
		err = errors.New("timeout waiting for tunnel to close")
	case err = <-done:
	}
	slog.Debug("Tunnel closed")
	return err
}

func (t *tunnel) Ctx() context.Context {
	return t.ctx
}

func (t *tunnel) Status() string {
	if t == nil {
		return ipc.StatusClosed
	}
	return t.status.Load().(string)
}
func (t *tunnel) ClashServer() *clashapi.Server {
	return t.clashServer
}

var errLibboxClosed = errors.New("libbox closed")

func (t *tunnel) reloadOptions(optsPath string) error {
	if t == nil {
		return nil
	}
	t.reloadAccess.Lock()
	defer t.reloadAccess.Unlock()

	if contextDone(t.ctx) {
		return t.ctx.Err() // tunnel is closing, ignore the reload
	}

	slog.Debug("Reloading server options", "path", optsPath)
	content, err := os.ReadFile(optsPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	svrs, err := json.UnmarshalExtendedContext[servers.Servers](box.BoxContext(), content)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}
	slog.Log(nil, internal.LevelTrace, "Parsed server options", "options", svrs)
	err = t.updateServers(svrs)
	if errors.Is(err, context.Canceled) {
		return nil // tunnel is closing, ignore the error
	}
	if err != nil && !errors.Is(err, errLibboxClosed) {
		return fmt.Errorf("update server configs: %w", err)
	}
	return nil
}

func (t *tunnel) updateServers(new servers.Servers) (err error) {
	var errs []error
	for _, group := range []string{servers.SGLantern, servers.SGUser} {
		err := t.updateGroup(group, new[group])
		if errors.Is(err, errLibboxClosed) {
			return err
		}
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (t *tunnel) updateGroup(group string, newOpts servers.Options) error {
	if len(newOpts.Outbounds) == 0 && len(newOpts.Endpoints) == 0 {
		slog.Debug("No outbounds or endpoints to update, skipping", "group", group)
		return nil
	}
	slog.Log(nil, internal.LevelTrace, "Updating servers", "group", group)

	mutGrpMgr := t.mutGrpMgr
	ctx := t.ctx
	router := service.FromContext[adapter.Router](ctx)

	autoTag := groupAutoTag(group)
	selector, selectorExists := mutGrpMgr.OutboundGroup(group)
	_, urltestExists := mutGrpMgr.OutboundGroup(autoTag)
	if !selectorExists || !urltestExists {
		// Yes, panic. And, yes, it's intentional. Both selector and URLtest should always exist
		// if the tunnel is running, so this is a "world no longer makes sense" situation. This
		// should be caught during testing and will not panic in release builds.
		slog.Log(
			nil, internal.LevelPanic, "selector or urltest group missing", "group", group,
			"selector_exists", selectorExists, "urltest_exists", urltestExists,
		)
		panic(fmt.Errorf(
			"selector or urltest group missing for %q. selector_exists=%v, urltest_exists=%v",
			group, selectorExists, urltestExists,
		))
	}

	var (
		removed = 0
		added   = 0
		errs    []error

		newTags = newOpts.AllTags()
	)

	if contextDone(ctx) {
		return ctx.Err()
	}

	// for each outbound/endpoint in current not in new, remove from group
	for _, tag := range selector.All() {
		if out, loaded := mutGrpMgr.OutboundGroup(tag); loaded {
			if _, isMutGroup := out.(lbA.MutableOutboundGroup); isMutGroup {
				continue // skip nested urltests
			}
		}
		if !slices.Contains(newTags, tag) {
			// remove from selector
			err := mutGrpMgr.RemoveFromGroup(group, tag)
			if err == nil {
				// remove from urltest
				err = mutGrpMgr.RemoveFromGroup(autoTag, tag)
			}
			if errors.Is(err, groups.ErrIsClosed) {
				return errLibboxClosed
			}
			if err != nil {
				errs = append(errs, err)
			} else {
				delete(t.optsMap, tag)
				removed++
			}
		}
	}

	// remove duplicates from newOpts before adding to avoid unnecessary reloads
	newOpts = removeDuplicates(t.optsMap, newOpts, group)

	// for each outbound/endpoint in new add to group
	for _, outbound := range newOpts.Outbounds {
		logger := t.logFactory.NewLogger("outbound/" + outbound.Tag + "[" + outbound.Type + "]")
		err := mutGrpMgr.CreateOutboundForGroup(
			ctx, router, logger, group, outbound.Tag, outbound.Type, outbound.Options,
		)
		if err == nil {
			// add to urltest
			err = mutGrpMgr.AddToGroup(autoTag, outbound.Tag)
		}
		if errors.Is(err, groups.ErrIsClosed) {
			return errLibboxClosed
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			t.optsMap[outbound.Tag], _ = json.MarshalContext(ctx, outbound)
			added++
		}
	}

	if contextDone(ctx) {
		return ctx.Err()
	}

	for _, endpoint := range newOpts.Endpoints {
		logger := t.logFactory.NewLogger("endpoint/" + endpoint.Tag + "[" + endpoint.Type + "]")
		err := mutGrpMgr.CreateEndpointForGroup(
			ctx, router, logger, group, endpoint.Tag, endpoint.Type, endpoint.Options,
		)
		if err == nil {
			// add to urltest
			err = mutGrpMgr.AddToGroup(autoTag, endpoint.Tag)
		}
		if errors.Is(err, groups.ErrIsClosed) {
			return errLibboxClosed
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			t.optsMap[endpoint.Tag], _ = json.MarshalContext(ctx, endpoint)
			added++
		}
	}

	slog.Debug("Updated servers in group", "group", group, "added", added, "removed", removed)
	return errors.Join(errs...)
}

func removeDuplicates(curr map[string][]byte, new servers.Options, group string) servers.Options {
	slog.Log(nil, internal.LevelTrace, "Removing duplicate outbounds/endpoints", "group", group)
	ctx := box.BoxContext()
	deduped := servers.Options{
		Outbounds: []O.Outbound{},
		Endpoints: []O.Endpoint{},
		Locations: map[string]lcommon.ServerLocation{},
	}
	var dropped []string
	for _, out := range new.Outbounds {
		if currOpts, exists := curr[out.Tag]; exists {
			if outBytes, _ := json.MarshalContext(ctx, out); bytes.Equal(currOpts, outBytes) {
				dropped = append(dropped, out.Tag)
				continue
			}
		}
		deduped.Outbounds = append(deduped.Outbounds, out)
		deduped.Locations[out.Tag] = new.Locations[out.Tag]
	}
	for _, ep := range new.Endpoints {
		if currOpts, exists := curr[ep.Tag]; exists {
			if epBytes, _ := json.MarshalContext(ctx, ep); bytes.Equal(currOpts, epBytes) {
				dropped = append(dropped, ep.Tag)
				continue
			}
		}
		deduped.Endpoints = append(deduped.Endpoints, ep)
		deduped.Locations[ep.Tag] = new.Locations[ep.Tag]
	}
	if len(dropped) > 0 {
		slog.Log(nil, internal.LevelDebug, "Dropped duplicate outbounds/endpoints", "group", group, "tags", dropped)
	}
	return deduped
}

func makeOutboundOptsMap(ctx context.Context, outbounds []O.Outbound, endpoints []O.Endpoint) map[string][]byte {
	optsMap := make(map[string][]byte)
	for _, out := range outbounds {
		optsMap[out.Tag], _ = json.MarshalContext(ctx, out)
	}
	for _, ep := range endpoints {
		optsMap[ep.Tag], _ = json.MarshalContext(ctx, ep)
	}
	return optsMap
}

func contextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
