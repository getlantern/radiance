package vpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	runtimeDebug "runtime/debug"
	"slices"
	"sync"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental"
	"github.com/sagernet/sing-box/experimental/libbox"
	sblog "github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

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
	"github.com/getlantern/radiance/vpn/memmon"
)

const defaultCloseTimeout = 10 * time.Second

type tunnel struct {
	ctx                  context.Context
	boxInstance          *sbox.Box
	clashServer          *clashServer
	selectionHistory     lbA.AutoSelectHistoryStorage
	selectionHistorySeed map[string]lbA.TagHistory
	logFactory           sblog.ObservableFactory

	dataPath string

	// optsMap is a map of current outbound/endpoint options JSON, used to deduplicate when adding
	// outbounds/endpoints
	optsMap     *lsync.TypedMap[string, []byte]
	mutGrpMgr   *groups.MutableGroupManager
	outboundMgr adapter.OutboundManager

	clientContextTracker *clientcontext.ClientContextInjector

	initialLanternTags []string

	// memoryMonitor starts during tunnel init in visibility-only mode.
	// On iOS, its executor is installed later, after the clash server exists.
	memoryMonitor *memmon.Monitor

	// connObserver, if non-nil, is attached to clashServer's tracker at connect to receive
	// connection-close pushes for telemetry.
	connObserver ConnObserver

	// outboundMu serializes the outbound mutators. Each does a read-modify-write
	// over clientContextTracker.MatchBounds(), which clones on read, so
	// concurrent mutators would silently drop each other's tag updates.
	outboundMu sync.Mutex

	// closeTimeout bounds how long close() waits for closers to finish; a zero
	// value uses defaultCloseTimeout.
	closeTimeout time.Duration

	cancel  context.CancelFunc
	closers []io.Closer
}

func (t *tunnel) start(ctx context.Context, options O.Options, platformIfce libbox.PlatformInterface, isRestart bool) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "tunnel.start",
		trace.WithAttributes(
			attribute.String("platform", common.Platform),
			attribute.Bool("is_restart", isRestart),
		))
	defer span.End()

	if err := t.init(ctx, options, platformIfce); err != nil {
		t.close()
		slog.Error("Failed to initialize tunnel", "error", err)
		return fmt.Errorf("initializing tunnel: %w", err)
	}

	if err := t.connect(ctx); err != nil {
		t.close()
		slog.Error("Failed to connect tunnel", "error", err)
		return fmt.Errorf("connecting tunnel: %w", err)
	}
	t.optsMap = makeOutboundOptsMap(t.ctx, options)
	return nil
}

// traceSpan wraps fn in a child span of the caller's context and records any
// error on the child span so failures show up per-phase in the trace.
func traceSpan(ctx context.Context, name string, fn func() error) error {
	_, span := otel.Tracer(tracerName).Start(ctx, name)
	defer span.End()
	err := fn()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (t *tunnel) init(ctx context.Context, options O.Options, platformIfce libbox.PlatformInterface) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "tunnel.init")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	// Must run before sbox.New, which acquires the cache flock.
	if err := consumeCacheClearMarker(t.dataPath); err != nil {
		slog.Warn("Failed to apply deferred tunnel cache clear", "path", t.dataPath, "error", err)
	}

	// Must overwrite the clash server constructor before calling sbox.New
	experimental.RegisterClashServerConstructor(newClashServer)

	slog.Log(nil, rlog.LevelTrace, "Initializing tunnel")

	t.ctx, t.cancel = context.WithCancel(context.Background())
	if common.IsMobile() {
		var monitorCloser io.Closer
		t.memoryMonitor, monitorCloser = startMemoryMonitor(t.ctx)
		t.closers = append(t.closers, monitorCloser)

		if common.IsIOS() {
			// Pin GOMEMLIMIT before the allocation-heavy libbox bring-up so GC
			// gets its turn during it. Only iOS needs this: it silently kills the
			// extension past a hard cap; Android applies no such restriction.
			setMobileMemoryLimits()
		}
	}
	if common.IsAndroid() {
		libbox.Setup(&libbox.SetupOptions{FixAndroidStack: true})
	}

	boxCtx := box.Context(t.ctx)
	if platformIfce != nil {
		boxCtx = service.ContextWith[adapter.PlatformInterface](boxCtx, libbox.NewPlatformInterfaceWrapper(platformIfce))
	}

	// Unbounded signaling must dial freddie outside the VPN tunnel or it
	// recursively re-enters itself. streamingRoundTripper forces kindling to
	// skip AMP (non-streamable) so freddie's long-poll genesis stream works.
	boxCtx = lbA.ContextWithDirectTransport(boxCtx, streamingRoundTripper{inner: kindling.HTTPClient().Transport})

	t.ctx = boxCtx
	t.logFactory = lblog.NewFactory(slog.Default().Handler())
	service.MustRegister[sblog.Factory](t.ctx, t.logFactory)

	t.selectionHistory = lbA.NewAutoSelectHistoryStorage()
	for tag, h := range t.selectionHistorySeed {
		entry := h
		t.selectionHistory.Store(tag, &entry)
	}
	service.MustRegister[lbA.AutoSelectHistoryStorage](t.ctx, t.selectionHistory)
	t.closers = append(t.closers, t.selectionHistory)

	slog.Log(nil, rlog.LevelTrace, "Creating box instance")
	var instance *sbox.Box
	if err := traceSpan(ctx, "sbox.New", func() error {
		var err error
		instance, err = sbox.New(sbox.Options{
			Context: t.ctx,
			Options: options,
		})
		return err
	}); err != nil {
		return fmt.Errorf("create box instance: %w", err)
	}
	cacheFile := service.FromContext[adapter.CacheFile](t.ctx)
	service.MustRegister[adapter.CacheFile](t.ctx, &cacheFileWrapper{CacheFile: cacheFile})

	// setup client info tracker
	outboundMgr := service.FromContext[adapter.OutboundManager](t.ctx)
	clientContextInjector := newClientContextInjector(outboundMgr, t.dataPath, t.initialLanternTags)
	service.MustRegisterPtr[clientcontext.ClientContextInjector](t.ctx, clientContextInjector)
	t.clientContextTracker = clientContextInjector
	router := service.FromContext[adapter.Router](t.ctx)
	router.AppendTracker(clientContextInjector)

	t.closers = append(t.closers, instance)
	t.boxInstance = instance

	slog.Info("Tunnel initialized")
	return nil
}

// Memory tuning for mobile devices, which have more constrained resources. iOS will silently
// kill the extension if it exceeds a hard cap (≈50 MB).
const (
	// mobileGCPercent is the soft cap for triggering the GC.
	mobileGCPercent = 50
	// mobileMemoryLimit is the GOMEMLIMIT soft cap. This needs to be below the iOS hard cap
	// to leave room for the non-Go side (Swift, cgo, etc.).
	mobileMemoryLimit = 40 * 1024 * 1024 // 40 MB
)

func setMobileMemoryLimits() {
	slog.Debug("Setting memory limits for mobile platform", "platform", common.Platform,
		"gc_percent", mobileGCPercent, "go_mem_limit", mobileMemoryLimit,
	)
	runtimeDebug.SetGCPercent(mobileGCPercent)
	runtimeDebug.SetMemoryLimit(mobileMemoryLimit)
}

func newClientContextInjector(outboundMgr adapter.OutboundManager, dataPath string, lanternTags []string) *clientcontext.ClientContextInjector {
	slog.Debug("Creating ClientContextInjector")
	infoFn := func() clientcontext.ClientInfo {
		return clientcontext.ClientInfo{
			DeviceID:    settings.GetString(settings.DeviceIDKey),
			Platform:    common.Platform,
			IsPro:       settings.IsPro(),
			CountryCode: settings.GetString(settings.CountryCodeKey),
			Version:     common.GetVersion(),
		}
	}
	// Only lantern servers support client context tracking, so only their tags
	// belong in the match bounds.
	matchBounds := clientcontext.MatchBounds{
		Inbound:  []string{"any"},
		Outbound: slices.Clone(lanternTags),
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

func (t *tunnel) connect(ctx context.Context) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "tunnel.connect")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	slog.Log(nil, rlog.LevelTrace, "Starting libbox service")

	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic starting libbox service", "panic", r)
			err = fmt.Errorf("panic starting libbox service: %v", r)
		}
	}()
	if err := traceSpan(ctx, "libbox.BoxService.Start", func() error {
		return t.boxInstance.Start()
	}); err != nil {
		slog.Error("Failed to start libbox service", "error", err)
		return fmt.Errorf("starting libbox service: %w", err)
	}
	slog.Debug("Libbox service started")

	t.clashServer = service.FromContext[adapter.ClashServer](t.ctx).(*clashServer)
	t.outboundMgr = service.FromContext[adapter.OutboundManager](t.ctx)
	t.clashServer.connTracker.SetObserver(t.connObserver)

	if common.IsIOS() {
		// Only iOS enforces a hard memory cap, so only it gets reclaim and
		// admission control. Android runs the monitor for visibility only.
		initExecutor(t.memoryMonitor, t.clashServer)
	}

	var mutGrpMgr *groups.MutableGroupManager
	if err := traceSpan(ctx, "newMutableGroupManager", func() error {
		var err error
		mutGrpMgr, err = newMutableGroupManager(
			t.ctx, t.logFactory.NewLogger("groupsManager"), t.clashServer.connTracker,
		)
		return err
	}); err != nil {
		return fmt.Errorf("creating mutable group manager: %w", err)
	}
	t.mutGrpMgr = mutGrpMgr
	// Prepend: mgm's removalQueue reads from libbox-managed state, so close it first.
	t.closers = append([]io.Closer{
		closerFunc(func() error { mutGrpMgr.Close(); return nil }),
	}, t.closers...)

	t.subscribeExhaustionSignal()

	slog.Info("Tunnel connection established")
	return nil
}

// subscribeExhaustionSignal bridges the auto group's exhaustion
// channel onto ExhaustionEvent so subscribers can react without the
// tunnel knowing about them.
func (t *tunnel) subscribeExhaustionSignal() {
	g, ok := t.mutGrpMgr.OutboundGroup(AutoSelectTag)
	if !ok {
		slog.Warn("auto group not found; exhaustion events disabled",
			"tag", AutoSelectTag)
		return
	}
	signaler, ok := g.(lbA.ExhaustionSignaler)
	if !ok {
		slog.Warn("auto group does not implement ExhaustionSignaler; exhaustion events disabled",
			"tag", AutoSelectTag)
		return
	}
	go t.emitExhaustionEvents(signaler.ExhaustionSignal())
}

func (t *tunnel) emitExhaustionEvents(ch <-chan struct{}) {
	for {
		select {
		case <-t.ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			events.Emit(ExhaustionEvent{})
		}
	}
}

func (t *tunnel) selectMode(mode string) error {
	if t.boxInstance == nil {
		return fmt.Errorf("tunnel not running")
	}

	if t.clashServer.Mode() != mode {
		t.clashServer.SetMode(mode)
		// Reset connections on a mode switch since the user explicitly chose to switch
		closeAllRouted(t.clashServer.connTracker)
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
	if t.cancel != nil {
		t.cancel()
	}

	closers := t.closers
	t.closers = nil
	t.boxInstance = nil

	done := make(chan error, 1)
	go func() {
		var errs []error
		for _, closer := range closers {
			slog.Log(nil, rlog.LevelTrace, "Closing tunnel resource", "type", fmt.Sprintf("%T", closer))
			errs = append(errs, closer.Close())
		}
		err := errors.Join(errs...)
		done <- err
		slog.Log(nil, rlog.LevelTrace, "Tunnel closers finished", "error", err)
	}()

	timeout := t.closeTimeout
	if timeout == 0 {
		timeout = defaultCloseTimeout
	}
	select {
	case <-time.After(timeout):
		slog.Warn("Timed out waiting for tunnel to close; closers still running")
		return errors.New("timeout waiting for tunnel to close")
	case err := <-done:
		return err
	}
}

var errLibboxClosed = errors.New("libbox closed")

func (t *tunnel) addOutbounds(list servers.ServerList) error {
	t.outboundMu.Lock()
	defer t.outboundMu.Unlock()
	return t.addOutboundsLocked(list)
}

// addOutboundsLocked adds the servers in list to the tunnel. The caller must
// hold t.outboundMu.
func (t *tunnel) addOutboundsLocked(list servers.ServerList) (err error) {
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
		// Iterate the full list, not the deduped newList: removeDuplicates drops
		// startup lantern servers that must stay bound, and the append-if-absent
		// guard below re-adds any the construction seed missed without regrowing
		// Outbound.
		lanternTags := make([]string, 0, len(list.Servers))
		for _, srv := range list.Servers {
			if srv.IsLantern && srv.Tag != "" {
				lanternTags = append(lanternTags, srv.Tag)
			}
		}
		if len(lanternTags) > 0 {
			slog.Log(nil, rlog.LevelTrace, "Merging lantern tags into ClientContextInjector")
			matchBounds := t.clientContextTracker.MatchBounds()
			for _, tag := range lanternTags {
				if !slices.Contains(matchBounds.Outbound, tag) {
					matchBounds.Outbound = append(matchBounds.Outbound, tag)
				}
			}
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
		slog.Info("Applying bandit URL overrides to auto-select group",
			"override_count", len(list.URLOverrides),
		)
	}
	if err := t.mutGrpMgr.SetURLOverrides(AutoSelectTag, list.URLOverrides); err != nil {
		slog.Warn("Failed to set URL overrides", "error", err)
	} else if len(list.URLOverrides) > 0 {
		// Trigger an immediate probe cycle when we have bandit overrides so
		// callback probes are hit within seconds of config receipt rather than
		// waiting for the next scheduled interval (3 min).
		if err := t.mutGrpMgr.CheckOutbounds(AutoSelectTag); err != nil {
			slog.Warn("Failed to trigger immediate probe cycle after bandit overrides", "error", err)
		} else {
			slog.Info("Triggered immediate probe cycle for bandit callbacks")
		}
	}

	slog.Debug("Added servers", "added", added)
	return errors.Join(errs...)
}

func (t *tunnel) removeOutbounds(tags []string) error {
	t.outboundMu.Lock()
	defer t.outboundMu.Unlock()
	return t.removeOutboundsLocked(tags)
}

// removeOutboundsLocked removes the outbounds with the given tags from the
// tunnel. The caller must hold t.outboundMu.
func (t *tunnel) removeOutboundsLocked(tags []string) error {
	var (
		mutGrpMgr = t.mutGrpMgr
		removed   []string
		errs      []error
	)
	for _, tag := range tags {
		if out, loaded := mutGrpMgr.OutboundGroup(tag); loaded {
			if _, isMutGroup := out.(lbA.MutableOutboundGroup); isMutGroup {
				continue // skip nested groups
			}
		}
		err := mutGrpMgr.RemoveFromGroup(ManualSelectTag, tag)
		if err == nil {
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
	t.outboundMu.Lock()
	defer t.outboundMu.Unlock()

	var errs []error
	outbounds := list.Outbounds()
	endpoints := list.Endpoints()
	if len(outbounds) == 0 && len(endpoints) == 0 && len(list.URLOverrides) == 0 {
		slog.Debug("No outbounds, endpoints, or bandit overrides to update, skipping")
		return nil
	}
	slog.Log(nil, rlog.LevelTrace, "Updating servers")

	selector, selectorExists := t.mutGrpMgr.OutboundGroup(ManualSelectTag)
	_, autoSelectExists := t.mutGrpMgr.OutboundGroup(AutoSelectTag)
	if !selectorExists || !autoSelectExists {
		slog.Error("Selector or auto-select group not found when updating outbounds")
		return errors.New("selector or auto-select group not found")
	}

	if contextDone(t.ctx) {
		return t.ctx.Err()
	}

	// collect tags present in the current group but absent from the new config
	newTags := list.Tags()
	// In manual mode, keep the selected outbound alive across config
	// refreshes so the user doesn't get redialed when the bandit excludes
	// it. Gated on mode because selector.Now() defaults to the first
	// outbound even with no manual selection.
	var pinnedTag string
	if t.clashServer != nil && t.clashServer.Mode() == ManualSelectTag {
		pinnedTag = selector.Now()
	}
	var toRemove []string
	for _, tag := range selector.All() {
		if tag == pinnedTag {
			continue
		}
		if !slices.Contains(newTags, tag) {
			toRemove = append(toRemove, tag)
		}
	}

	// Add new outbounds first, before removing old ones. If all new
	// outbounds fail to load (e.g. invalid config), we keep the old
	// working outbounds to maintain connectivity.
	addErr := t.addOutboundsLocked(list)
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
		if err := t.removeOutboundsLocked(toRemove); errors.Is(err, errLibboxClosed) {
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

func makeOutboundOptsMap(ctx context.Context, options O.Options) *lsync.TypedMap[string, []byte] {
	var optsMap lsync.TypedMap[string, []byte]
	for _, out := range options.Outbounds {
		b, _ := json.MarshalContext(ctx, out)
		optsMap.Store(out.Tag, b)
	}
	for _, ep := range options.Endpoints {
		b, _ := json.MarshalContext(ctx, ep)
		optsMap.Store(ep.Tag, b)
	}
	return &optsMap
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

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

// cacheFileWrapper suppresses libbox's persistence of the selected outbound
// so BoxOptions.InitialServer controls the selection on each connect rather
// than a stale value from disk.
type cacheFileWrapper struct {
	adapter.CacheFile
}

func (c *cacheFileWrapper) LoadSelected(_ string) string {
	return ""
}

func (c *cacheFileWrapper) StoreSelected(_, _ string) error {
	return nil
}
