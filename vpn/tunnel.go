package vpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	runtimeDebug "runtime/debug"
	"slices"
	"sync/atomic"
	"time"

	lsync "github.com/getlantern/common/sync"
	box "github.com/getlantern/lantern-box"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/adapter/groups"
	lblog "github.com/getlantern/lantern-box/log"
	"github.com/getlantern/lantern-box/tracker/clientcontext"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling"
	rlog "github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/conntrack"
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
	optsMap     *lsync.TypedMap[string, []byte]
	mutGrpMgr   *groups.MutableGroupManager
	outboundMgr adapter.OutboundManager

	clientContextTracker *clientcontext.ClientContextInjector

	status  atomic.Value // VPNStatus
	cancel  context.CancelFunc
	closers []io.Closer
}

func (t *tunnel) start(options string, platformIfce libbox.PlatformInterface) (err error) {
	if t.status.Load() != Restarting {
		t.setStatus(Connecting, nil)
	}
	// Unbounded signaling must dial freddie outside the VPN tunnel or it
	// recursively re-enters itself. streamingRoundTripper forces kindling to
	// skip AMP (non-streamable) so freddie's long-poll genesis stream works.
	baseCtx := lbA.ContextWithDirectTransport(box.BaseContext(), streamingRoundTripper{inner: kindling.HTTPClient().Transport})
	t.ctx, t.cancel = context.WithCancel(baseCtx)
	defer func() {
		if err != nil {
			t.setStatus(ErrorStatus, err)
		}
	}()

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
	t.setStatus(Connected, nil)
	t.optsMap = makeOutboundOptsMap(t.ctx, options)
	return nil
}

func (t *tunnel) init(options string, platformIfce libbox.PlatformInterface) error {
	slog.Log(nil, rlog.LevelTrace, "Initializing tunnel")

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

	slog.Log(nil, rlog.LevelTrace, "Setting up libbox", "setup_options", setupOpts)
	if err := libbox.Setup(setupOpts); err != nil {
		return fmt.Errorf("setup libbox: %w", err)
	}

	t.logFactory = lblog.NewFactory(slog.Default().Handler())
	service.MustRegister[sblog.Factory](t.ctx, t.logFactory)

	slog.Log(nil, rlog.LevelTrace, "Creating libbox service")
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

	// history := service.PtrFromContext[urltest.HistoryStorage](t.ctx)
	// if err := loadURLTestHistory(history, filepath.Join(dataPath, urlTestHistoryFileName)); err != nil {
	// 	return fmt.Errorf("load urltest history: %w", err)
	// }

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
	// Outbound match bounds start empty and are populated when lantern servers are added via
	// addOutbounds. Only lantern servers support client context tracking.
	matchBounds := clientcontext.MatchBounds{
		Inbound:  []string{"any"},
		Outbound: []string{},
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
	slog.Log(nil, rlog.LevelTrace, "Starting libbox service")

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
	t.outboundMgr = service.FromContext[adapter.OutboundManager](t.ctx)

	mutGrpMgr, err := newMutableGroupManager(
		t.ctx, t.logFactory.NewLogger("groupsManager"), t.clashServer.TrafficManager(),
	)
	if err != nil {
		t.close()
		return fmt.Errorf("creating mutable group manager: %w", err)
	}
	t.mutGrpMgr = mutGrpMgr

	slog.Info("Tunnel connection established")
	return nil
}

func (t *tunnel) selectMode(mode string) error {
	if status := t.Status(); status != Connected {
		return fmt.Errorf("tunnel not running: status %v", status)
	}

	if t.clashServer.Mode() != mode {
		t.clashServer.SetMode(mode)
		conntrack.Close()
		go func() {
			time.Sleep(time.Second)
			runtimeDebug.FreeOSMemory()
		}()
	}
	return nil
}

func (t *tunnel) selectOutbound(tag string) error {
	if err := t.selectMode(ManualSelectTag); err != nil {
		return err
	}

	outboundMgr := service.FromContext[adapter.OutboundManager](t.ctx)
	outbound, loaded := outboundMgr.Outbound(ManualSelectTag)
	if !loaded {
		return fmt.Errorf("manual select group not found")
	}
	outbound.(Selector).SelectOutbound(tag)
	return nil
}

func (t *tunnel) close() error {
	if t.status.Load() != Restarting {
		t.setStatus(Disconnecting, nil)
	}
	if t.cancel != nil {
		t.cancel()
	}

	done := make(chan error)
	go func() {
		var errs []error
		for _, closer := range t.closers {
			slog.Log(nil, rlog.LevelTrace, "Closing tunnel resource", "type", fmt.Sprintf("%T", closer))
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
	if t.status.Load() != Restarting {
		t.setStatus(Disconnected, nil)
	}
	return err
}

func (t *tunnel) Status() VPNStatus {
	return t.status.Load().(VPNStatus)
}

func (t *tunnel) setStatus(status VPNStatus, err error) {
	t.status.Store(status)
	evt := StatusUpdateEvent{Status: status}
	if err != nil {
		evt.Error = err.Error()
	}
	events.Emit(evt)
}

var errLibboxClosed = errors.New("libbox closed")

func (t *tunnel) addOutbounds(list servers.ServerList) (err error) {
	outbounds := list.Outbounds()
	endpoints := list.Endpoints()
	if len(outbounds) == 0 && len(endpoints) == 0 {
		slog.Debug("No outbounds or endpoints to add")
		return nil
	}

	slog.Info("Adding servers", "tags", list.Tags())
	// remove duplicates from newOpts before adding to avoid unnecessary reloads
	newList := removeDuplicates(t.ctx, t.optsMap, list)
	newOutbounds := newList.Outbounds()
	newEndpoints := newList.Endpoints()

	ctx := t.ctx
	router := service.FromContext[adapter.Router](ctx)

	var errs []error
	if t.clientContextTracker != nil {
		// preemptively merge the new lantern tags into the clientContextInjector match bounds to
		// capture any new connections before finished adding the servers.
		lanternTags := make([]string, 0, len(newList.Servers))
		for _, srv := range newList.Servers {
			if srv.IsLantern {
				lanternTags = append(lanternTags, srv.Tag)
			}
		}
		if len(lanternTags) > 0 {
			slog.Log(nil, rlog.LevelTrace, "Temporarily merging new lantern tags into ClientContextInjector")
			matchBounds := t.clientContextTracker.MatchBounds()
			matchBounds.Outbound = append(matchBounds.Outbound, lanternTags...)
			t.clientContextTracker.SetBounds(matchBounds)
		}
		defer func() {
			if errors.Is(err, errLibboxClosed) {
				return
			}
			// Remove any lantern tags that failed to load from the match bounds.
			mb := t.clientContextTracker.MatchBounds()
			mb.Outbound = slices.DeleteFunc(mb.Outbound, func(tag string) bool {
				_, loaded := t.optsMap.Load(tag)
				return slices.Contains(lanternTags, tag) && !loaded
			})
			t.clientContextTracker.SetBounds(mb)
		}()
	}

	var (
		mutGrpMgr = t.mutGrpMgr
		added     = 0
	)
	for _, outbound := range newOutbounds {
		logger := t.logFactory.NewLogger("outbound/" + outbound.Tag + "[" + outbound.Type + "]")
		err := mutGrpMgr.CreateOutboundForGroup(
			ctx, router, logger, ManualSelectTag, outbound.Tag, outbound.Type, outbound.Options,
		)
		if err == nil {
			err = mutGrpMgr.AddToGroup(AutoSelectTag, outbound.Tag)
		}
		if errors.Is(err, groups.ErrIsClosed) {
			return errLibboxClosed
		}
		if err != nil {
			slog.Warn("Failed to load outbound",
				"tag", outbound.Tag,
				"type", outbound.Type,
				"error", err,
			)
			errs = append(errs, err)
		} else {
			b, _ := json.MarshalContext(ctx, outbound)
			t.optsMap.Store(outbound.Tag, b)
			added++
		}
	}

	if contextDone(ctx) {
		return ctx.Err()
	}

	for _, endpoint := range newEndpoints {
		logger := t.logFactory.NewLogger("endpoint/" + endpoint.Tag + "[" + endpoint.Type + "]")
		err := mutGrpMgr.CreateEndpointForGroup(
			ctx, router, logger, ManualSelectTag, endpoint.Tag, endpoint.Type, endpoint.Options,
		)
		if err == nil {
			err = mutGrpMgr.AddToGroup(AutoSelectTag, endpoint.Tag)
		}
		if errors.Is(err, groups.ErrIsClosed) {
			return errLibboxClosed
		}
		if err != nil {
			slog.Warn("Failed to load endpoint",
				"tag", endpoint.Tag,
				"type", endpoint.Type,
				"error", err,
			)
			errs = append(errs, err)
		} else {
			b, _ := json.MarshalContext(ctx, endpoint)
			t.optsMap.Store(endpoint.Tag, b)
			added++
		}
	}

	if len(list.URLOverrides) > 0 {
		slog.Info("Applying bandit URL overrides to URL test group",
			"override_count", len(list.URLOverrides),
		)
	}
	if err := t.mutGrpMgr.SetURLOverrides(AutoSelectTag, list.URLOverrides); err != nil {
		slog.Warn("Failed to set URL overrides", "error", err)
	} else if len(list.URLOverrides) > 0 {
		// Trigger an immediate URL test cycle when we have bandit overrides so
		// callback probes are hit within seconds of config receipt rather than
		// waiting for the next scheduled interval (3 min).
		if err := t.mutGrpMgr.CheckOutbounds(AutoSelectTag); err != nil {
			slog.Warn("Failed to trigger immediate URL test after bandit overrides", "error", err)
		} else {
			slog.Info("Triggered immediate URL test for bandit callbacks")
		}
	}

	slog.Debug("Added servers", "added", added)
	return errors.Join(errs...)
}

func (t *tunnel) removeOutbounds(tags []string) error {
	var (
		mutGrpMgr = t.mutGrpMgr
		removed   []string
		errs      []error
	)
	for _, tag := range tags {
		if out, loaded := mutGrpMgr.OutboundGroup(tag); loaded {
			if _, isMutGroup := out.(lbA.MutableOutboundGroup); isMutGroup {
				continue // skip nested urltests
			}
		}
		err := mutGrpMgr.RemoveFromGroup(ManualSelectTag, tag)
		if err == nil {
			// remove from urltest
			err = mutGrpMgr.RemoveFromGroup(AutoSelectTag, tag)
		}
		if errors.Is(err, groups.ErrIsClosed) {
			return errLibboxClosed
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			t.optsMap.Delete(tag)
			removed = append(removed, tag)
		}
	}
	if t.clientContextTracker != nil && len(removed) > 0 {
		mb := t.clientContextTracker.MatchBounds()
		mb.Outbound = slices.DeleteFunc(mb.Outbound, func(s string) bool {
			return slices.Contains(removed, s)
		})
		t.clientContextTracker.SetBounds(mb)
	}
	slog.Debug("Removed servers", "removed", len(removed))
	return errors.Join(errs...)
}

func (t *tunnel) updateOutbounds(list servers.ServerList) error {
	var errs []error
	outbounds := list.Outbounds()
	endpoints := list.Endpoints()
	if len(outbounds) == 0 && len(endpoints) == 0 && len(list.URLOverrides) == 0 {
		slog.Debug("No outbounds, endpoints, or bandit overrides to update, skipping")
		return nil
	}
	slog.Log(nil, rlog.LevelTrace, "Updating servers")

	selector, selectorExists := t.mutGrpMgr.OutboundGroup(ManualSelectTag)
	_, urltestExists := t.mutGrpMgr.OutboundGroup(AutoSelectTag)
	if !selectorExists || !urltestExists {
		slog.Error("Selector or URL test group not found when updating outbounds")
		return errors.New("selector or url test group not found")
	}

	if contextDone(t.ctx) {
		return t.ctx.Err()
	}

	// collect tags present in the current group but absent from the new config
	newTags := list.Tags()
	var toRemove []string
	for _, tag := range selector.All() {
		if !slices.Contains(newTags, tag) {
			toRemove = append(toRemove, tag)
		}
	}

	// Add new outbounds first, before removing old ones. If all new
	// outbounds fail to load (e.g. invalid config), we keep the old
	// working outbounds to maintain connectivity.
	addErr := t.addOutbounds(list)
	if errors.Is(addErr, errLibboxClosed) {
		return addErr
	}
	if addErr != nil {
		errs = append(errs, addErr)
	}

	// Check if any new outbound actually loaded
	hasNewOutbound := false
	for _, tag := range newTags {
		if slices.Contains(selector.All(), tag) {
			hasNewOutbound = true
			break
		}
	}

	if hasNewOutbound {
		if err := t.removeOutbounds(toRemove); errors.Is(err, errLibboxClosed) {
			return err
		} else if err != nil {
			errs = append(errs, err)
		}
	} else {
		slog.Warn("All new outbounds failed to load, keeping old outbounds",
			"failed_tags", newTags, "would_remove_tags", toRemove)
	}

	return errors.Join(errs...)
}

func removeDuplicates(ctx context.Context, curr *lsync.TypedMap[string, []byte], list servers.ServerList) servers.ServerList {
	slog.Log(nil, rlog.LevelTrace, "Removing duplicate outbounds/endpoints")
	var deduped []*servers.Server
	var dropped []string
	for _, srv := range list.Servers {
		if currOpts, exists := curr.Load(srv.Tag); exists {
			if srvBytes, _ := json.MarshalContext(ctx, srv.Options); bytes.Equal(currOpts, srvBytes) {
				dropped = append(dropped, srv.Tag)
				continue
			}
		}
		deduped = append(deduped, srv)
	}
	if len(dropped) > 0 {
		slog.Debug("Dropped duplicate outbounds/endpoints", "tags", dropped)
	}
	return servers.ServerList{
		Servers:      deduped,
		URLOverrides: list.URLOverrides,
	}
}

func makeOutboundOptsMap(ctx context.Context, options string) *lsync.TypedMap[string, []byte] {
	// we can ignore the error here because we would have already failed if we couldn't parse the
	// options JSON in the first place
	opts, _ := json.UnmarshalExtendedContext[O.Options](ctx, []byte(options))
	var optsMap lsync.TypedMap[string, []byte]
	for _, out := range opts.Outbounds {
		b, _ := json.MarshalContext(ctx, out)
		optsMap.Store(out.Tag, b)
	}
	for _, ep := range opts.Endpoints {
		b, _ := json.MarshalContext(ctx, ep)
		optsMap.Store(ep.Tag, b)
	}
	return &optsMap
}

func contextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// streamingRoundTripper defaults Accept to text/event-stream so kindling's
// race pipeline drops non-streamable transports (AMP) that would otherwise
// buffer freddie's long-poll body and break broflake's genesis subscription.
type streamingRoundTripper struct {
	inner http.RoundTripper
}

func (s streamingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := s.inner.RoundTrip(req)
	if err != nil {
		slog.Error("unbounded signaling RoundTrip error",
			slog.String("url", req.URL.String()),
			slog.Any("error", err))
		return nil, err
	}
	return resp, nil
}
