package vpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	lcommon "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/adapter/groups"
	lblog "github.com/getlantern/lantern-box/log"
	"github.com/getlantern/lantern-box/tracker/clientcontext"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn/ipc"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
	sblog "github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

type tunnel struct {
	ctx         context.Context
	lbService   *libbox.BoxService
	clashServer *clashapi.Server
	logFactory  sblog.ObservableFactory

	dataPath string

	// optsMap is a map of current outbound/endpoint options JSON, used to deduplicate when adding
	// outbounds/endpoints
	optsMap   map[string][]byte
	mutGrpMgr *groups.MutableGroupManager

	clientContextTracker *clientcontext.ClientContextInjector

	status  atomic.Value
	cancel  context.CancelFunc
	closers []io.Closer
}

func (t *tunnel) start(options string, platformIfce libbox.PlatformInterface) error {
	t.status.Store(ipc.Connecting)
	t.ctx, t.cancel = context.WithCancel(box.BaseContext())

	if err := t.init(options, platformIfce); err != nil {
		t.close()
		slog.Error("Failed to initialize tunnel", "error", err)
		return fmt.Errorf("initializing tunnel: %w", err)
	}

	if err := t.connect(); err != nil {
		t.close()
		slog.Error("Failed to connect tunnel", "error", err)
		return fmt.Errorf("connecting tunnel: %w", err)
	}
	t.status.Store(ipc.Connected)
	t.optsMap = makeOutboundOptsMap(t.ctx, options)
	return nil
}

func (t *tunnel) init(options string, platformIfce libbox.PlatformInterface) error {
	slog.Log(nil, internal.LevelTrace, "Initializing tunnel")

	// setup libbox service
	dataPath := t.dataPath
	setupOpts := &libbox.SetupOptions{
		BasePath: dataPath,
		TempPath: filepath.Join(dataPath, "temp"),
	}
	if !common.IsWindows() {
		setupOpts.WorkingPath = dataPath
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

	slog.Log(nil, internal.LevelTrace, "Creating libbox service")
	lb, err := libbox.NewServiceWithContext(t.ctx, options, platformIfce)
	if err != nil {
		return fmt.Errorf("create libbox service: %w", err)
	}

	// setup client info tracker
	outboundMgr := service.FromContext[adapter.OutboundManager](t.ctx)
	clientContextInjector := newClientContextInjector(outboundMgr, dataPath)
	service.MustRegisterPtr[clientcontext.ClientContextInjector](t.ctx, clientContextInjector)
	t.clientContextTracker = clientContextInjector
	router := service.FromContext[adapter.Router](t.ctx)
	router.AppendTracker(clientContextInjector)

	t.closers = append(t.closers, lb)
	t.lbService = lb

	history := service.PtrFromContext[urltest.HistoryStorage](t.ctx)
	if err := loadURLTestHistory(history, filepath.Join(dataPath, urlTestHistoryFileName)); err != nil {
		return fmt.Errorf("load urltest history: %w", err)
	}

	// set memory limit for Android and iOS
	switch common.Platform {
	case "android", "ios":
		slog.Debug("Setting memory limit for mobile platform", "platform", common.Platform)
		libbox.SetMemoryLimit(true)
	default:
	}

	slog.Info("Tunnel initializated")
	return nil
}

func newClientContextInjector(outboundMgr adapter.OutboundManager, dataPath string) *clientcontext.ClientContextInjector {
	slog.Debug("Creating ClientContextInjector")
	infoFn := func() clientcontext.ClientInfo {
		return clientcontext.ClientInfo{
			DeviceID:    settings.GetString(settings.DeviceIDKey),
			Platform:    common.Platform,
			IsPro:       settings.IsPro(),
			CountryCode: settings.GetString(settings.CountryCodeKey),
			Version:     common.Version,
		}
	}
	matchBounds := clientcontext.MatchBounds{
		Inbound:  []string{"any"},
		Outbound: []string{},
	}
	if outbound, exists := outboundMgr.Outbound(servers.SGLantern); exists {
		// Note: this should only contain lantern outbounds with servers that support client context
		// tracking. otherwise, the connections will fail.
		tags := outbound.(adapter.OutboundGroup).All()
		matchBounds.Outbound = append(tags, servers.SGLantern, groupAutoTag(servers.SGLantern))
	}
	return clientcontext.NewClientContextInjector(infoFn, matchBounds)
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

	t.clashServer = service.FromContext[adapter.ClashServer](t.ctx).(*clashapi.Server)

	mutGrpMgr, err := newMutableGroupManager(
		t.ctx, t.logFactory.NewLogger("groupsManager"), t.clashServer.TrafficManager(),
	)
	if err != nil {
		return fmt.Errorf("creating mutable group manager: %w", err)
	}
	t.mutGrpMgr = mutGrpMgr

	slog.Info("Tunnel connection established")
	return nil
}

func (t *tunnel) selectOutbound(group, tag string) error {
	if status := t.Status(); status != ipc.Connected {
		return fmt.Errorf("tunnel not running: status %v", status)
	}

	t.clashServer.SetMode(group)
	if tag == "" {
		return nil
	}
	outboundMgr := service.FromContext[adapter.OutboundManager](t.ctx)
	outbound, loaded := outboundMgr.Outbound(group)
	if !loaded {
		return fmt.Errorf("selector group not found: %s", group)
	}
	outbound.(ipc.Selector).SelectOutbound(tag)
	return nil
}

func (t *tunnel) close() error {
	if t.cancel != nil {
		t.cancel()
	}

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

	t.closers = nil
	t.lbService = nil
	t.status.Store(ipc.Disconnected)
	return err
}

func (t *tunnel) Status() ipc.VPNStatus {
	return t.status.Load().(ipc.VPNStatus)
}

var errLibboxClosed = errors.New("libbox closed")

func (t *tunnel) addOutbounds(group string, options servers.Options) error {
	if len(options.Outbounds) == 0 && len(options.Endpoints) == 0 {
		slog.Debug("No outbounds or endpoints to add", "group", group)
		return nil
	}

	slog.Info("Adding servers to group", "group", group, "tags", options.AllTags())
	// remove duplicates from newOpts before adding to avoid unnecessary reloads
	newOptions := removeDuplicates(t.ctx, t.optsMap, options, group)

	ctx := t.ctx
	router := service.FromContext[adapter.Router](ctx)

	var errs []error
	if group == servers.SGLantern && t.clientContextTracker != nil {
		// preemptively merge the new lantern tags into the clientContextInjector match bounds to
		// capture any new connections before finished adding the servers.
		if tags := options.AllTags(); len(tags) > 0 {
			slog.Log(nil, internal.LevelTrace, "Temporarily merging new lantern tags into ClientContextInjector")
			matchBounds := t.clientContextTracker.MatchBounds()
			matchBounds.Outbound = append(matchBounds.Outbound, tags...)
			t.clientContextTracker.SetBounds(matchBounds)
		}
		defer func() {
			if len(errs) > 0 {
				// if there were errors adding the servers, we may have added some but not all of the
				// new tags to the clientContextInjector match bounds.
				t.updateClientContextTracker()
			}
		}()
	}

	var (
		mutGrpMgr = t.mutGrpMgr
		autoTag   = groupAutoTag(group)
		added     = 0
	)
	// for each outbound/endpoint in new add to group
	for _, outbound := range newOptions.Outbounds {
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

	for _, endpoint := range newOptions.Endpoints {
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
	slog.Debug("Added servers to group", "group", group, "added", added)
	return errors.Join(errs...)
}

func (t *tunnel) removeOutbounds(group string, tags []string) error {
	var (
		mutGrpMgr = t.mutGrpMgr
		autoTag   = groupAutoTag(group)
		removed   = 0
		errs      []error
	)
	for _, tag := range tags {
		if out, loaded := mutGrpMgr.OutboundGroup(tag); loaded {
			if _, isMutGroup := out.(lbA.MutableOutboundGroup); isMutGroup {
				continue // skip nested urltests
			}
		}
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
	if t.clientContextTracker != nil {
		t.updateClientContextTracker()
	}
	slog.Debug("Removed servers from group", "group", group, "removed", removed)
	return errors.Join(errs...)
}

func (t *tunnel) updateClientContextTracker() {
	outboundMgr := service.FromContext[adapter.OutboundManager](t.ctx)
	outbound, exists := outboundMgr.Outbound(servers.SGLantern)
	if !exists {
		return
	}
	outGroup := outbound.(adapter.OutboundGroup)
	slog.Debug("Setting updated lantern tags into ClientContextInjector")
	t.clientContextTracker.SetBounds(clientcontext.MatchBounds{
		Inbound:  []string{"any"},
		Outbound: append(outGroup.All(), servers.SGLantern, groupAutoTag(servers.SGLantern)),
	})
}

func (t *tunnel) updateOutbounds(new servers.Servers) error {
	var errs []error
	for _, group := range []string{servers.SGLantern, servers.SGUser} {
		newOpts := new[group]
		if len(newOpts.Outbounds) == 0 && len(newOpts.Endpoints) == 0 {
			slog.Debug("No outbounds or endpoints to update, skipping", "group", group)
			continue
		}
		slog.Log(nil, internal.LevelTrace, "Updating servers", "group", group)

		autoTag := groupAutoTag(group)
		selector, selectorExists := t.mutGrpMgr.OutboundGroup(group)
		_, urltestExists := t.mutGrpMgr.OutboundGroup(autoTag)
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

		if contextDone(t.ctx) {
			return t.ctx.Err()
		}

		// collect tags present in the current group but absent from the new config
		newTags := newOpts.AllTags()
		var toRemove []string
		for _, tag := range selector.All() {
			if !slices.Contains(newTags, tag) {
				toRemove = append(toRemove, tag)
			}
		}

		if err := t.removeOutbounds(group, toRemove); errors.Is(err, errLibboxClosed) {
			return err
		} else if err != nil {
			errs = append(errs, err)
		}
		if err := t.addOutbounds(group, newOpts); errors.Is(err, errLibboxClosed) {
			return err
		} else if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func removeDuplicates(ctx context.Context, curr map[string][]byte, new servers.Options, group string) servers.Options {
	slog.Log(nil, internal.LevelTrace, "Removing duplicate outbounds/endpoints", "group", group)
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

func makeOutboundOptsMap(ctx context.Context, options string) map[string][]byte {
	// we can ignore the error here because we would have already failed if we couldn't parse the
	// options JSON in the first place
	opts, _ := json.UnmarshalExtendedContext[O.Options](ctx, []byte(options))
	optsMap := make(map[string][]byte)
	for _, out := range opts.Outbounds {
		optsMap[out.Tag], _ = json.MarshalContext(ctx, out)
	}
	for _, ep := range opts.Endpoints {
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
