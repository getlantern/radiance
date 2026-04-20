package vpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	runtimeDebug "runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	broflakeCommon "github.com/getlantern/broflake/common"
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
	"github.com/sagernet/sing/common/control"
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
	// Redirect broflake's internal debug logger (consumer/producer FSM
	// state transitions, STUN cache population, ICE/peer-connection state
	// changes, datachannel open/close) from its default os.Stderr target
	// to our structured slog so the messages land in
	// /Users/Shared/Lantern/Logs/lantern.log instead of the system
	// extension's stderr (which on macOS goes nowhere the user can see).
	// Each broflake Debugf call becomes one slog.Info line tagged
	// subsys=broflake. Idempotent — broflakeCommon guards with a mutex,
	// so repeated starts just re-point to the same writer.
	broflakeCommon.SetDebugLogger(stdlog.New(&broflakeSlogWriter{}, "", 0))
	// Note: a single broflakeSlogWriter instance persists for the life of
	// this process regardless of VPN restarts. SetDebugLogger is
	// idempotent; repeated starts overwrite the logger but each instance
	// carries its own rate-limit state, which is fine — cross-restart
	// rate-limiting isn't useful here.
	// Unbounded signaling (and any other outbound that reads this value) must
	// dial freddie outside the user's VPN tunnel, otherwise it recursively
	// re-enters itself. kindling's RoundTripper dials via the physical
	// interface and blocks until kindling init completes.
	//
	// We wrap it in a streaming-aware transport: kindling's race pipeline
	// includes a non-streamable AMP transport that can win the race and
	// buffer the full response body. Freddie's genesis endpoint is a
	// long-poll SSE-style stream, so a buffered responder returns an
	// immediate short body and the broflake consumer FSM sees EOF before
	// any genesis message arrives, restarting forever without ever
	// sending an offer. Setting Accept: text/event-stream on freddie
	// requests makes kindling skip AMP and race only the streamable
	// transports (fronted, smart), so the stream stays open.
	baseCtx := lbA.ContextWithDirectTransport(box.BaseContext(), &streamingRoundTripper{inner: kindling.HTTPClient().Transport})
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

// streamingRoundTripper wraps an inner RoundTripper and sets
// `Accept: text/event-stream` on outgoing requests that don't already have
// an Accept header. This is specifically to work around kindling's race
// pipeline: the AMP transport is non-streamable, so if it wins the race
// against fronted/smart it buffers the full response body — which breaks
// freddie's long-poll genesis subscription. Kindling filters non-streamable
// transports only when the request Accept header is text/event-stream, so
// we force it here.
//
// We don't override an already-set Accept (some callers may legitimately
// ask for other content types); callers that expect streaming but omit
// Accept get the streaming-friendly default, which matches what SSE
// clients typically send anyway.
type streamingRoundTripper struct {
	inner http.RoundTripper

	// freddieTransport short-circuits requests to
	// `https://df.iantem.io/freddie/...` — broflake's signaling endpoint —
	// around kindling entirely. kindling's race pipeline includes a
	// "smart" transport that resolves via stdlib DNS before dialing; on
	// a Lantern client with the TUN up, that stdlib lookup ends up
	// querying the TUN's fakeip resolver (10.10.1.2:53), which loops
	// back into the extension and times out. When fronted or amp lose
	// the race, the whole POST fails, and broflake's consumer state 3
	// treats an ICE-candidate signaling failure as fatal — it closes
	// the peer connection and the datachannel never comes up.
	//
	// freddieTransport uses a dialer bound to the physical interface
	// (via the same NetworkManager ProtectFunc rtcNet uses for UDP) and
	// a Resolver that talks to a public DNS server (1.1.1.1) on that
	// same interface. No TUN traffic, no kindling race — one predictable
	// code path for every freddie round-trip.
	freddieTransport http.RoundTripper
	initFreddieOnce  sync.Once
}

// broflakeSlogWriter adapts broflake's log.Logger.Print* output to our
// slog. Broflake writes one complete line per call, terminated with
// "\n"; we strip that and forward as a single slog.Info record tagged
// subsys=broflake so it's easy to filter. This is the only way to see
// the broflake FSM state machine from inside the sandboxed system
// extension — broflake writes to os.Stderr by default, and the
// extension's stderr isn't attached to anything user-visible on macOS.
//
// Spammy messages (broflake's QUICLayer.Close-then-spin-loop bug in
// ListenAndMaintainQUICConnection, which emits "QUIC listener error
// (context canceled), closing!" thousands of times per second after a
// single tunnel teardown) are rate-limited to one line per second so
// the real signal isn't buried under disk-I/O-bound noise. Identified
// here by exact substring — if the root cause gets fixed upstream,
// the filter becomes a no-op.
type broflakeSlogWriter struct {
	lastSpamLog atomic.Int64 // unix-nanos of the most recent "spam" line logged
}

const broflakeSpamPattern = "QUIC listener error (context canceled), closing!"

func (b *broflakeSlogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg == "" {
		return len(p), nil
	}
	if strings.Contains(msg, broflakeSpamPattern) {
		now := time.Now().UnixNano()
		last := b.lastSpamLog.Load()
		if now-last < int64(time.Second) {
			return len(p), nil
		}
		b.lastSpamLog.Store(now)
		slog.Info(msg+" (rate-limited: 1/s)", "subsys", "broflake")
		return len(p), nil
	}
	slog.Info(msg, "subsys", "broflake")
	return len(p), nil
}

func (s *streamingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Accept", "text/event-stream")
	}
	transport := s.inner
	transportName := "kindling"
	if req.URL != nil && req.URL.Host == "df.iantem.io" {
		s.initFreddieOnce.Do(func() {
			s.freddieTransport = newFreddieTransport(req.Context())
		})
		transport = s.freddieTransport
		transportName = "freddie-direct"
	}
	slog.Info("unbounded signaling RoundTrip start",
		slog.String("method", req.Method),
		slog.String("url", req.URL.String()),
		slog.String("accept", req.Header.Get("Accept")),
		slog.String("transport", transportName))
	start := time.Now()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		slog.Error("unbounded signaling RoundTrip error",
			slog.String("url", req.URL.String()),
			slog.String("transport", transportName),
			slog.Duration("duration", time.Since(start)),
			slog.Any("error", err))
		return nil, err
	}
	slog.Info("unbounded signaling RoundTrip ok",
		slog.String("url", req.URL.String()),
		slog.String("transport", transportName),
		slog.Int("status", resp.StatusCode),
		slog.String("content_length", resp.Header.Get("Content-Length")),
		slog.String("transfer_encoding", strings.Join(resp.TransferEncoding, ",")),
		slog.Duration("duration_to_headers", time.Since(start)))
	return resp, nil
}

// bindEgressToPhysicalInterface returns a net.Dialer/ListenConfig
// Control function that binds new sockets to the platform's default
// physical interface (IP_BOUND_IF on macOS/iOS, SO_BINDTODEVICE on
// Linux/Android). Same logic sing-box's NewDefault uses when
// auto_detect_interface is active — we reach for it here directly so
// we can build control-plane dialers outside the sing-box outbound
// graph. Returns nil when no NetworkManager is registered on ctx
// (tests), letting the socket follow the routing table unmodified.
func bindEgressToPhysicalInterface(ctx context.Context) control.Func {
	nm := service.FromContext[adapter.NetworkManager](ctx)
	if nm == nil {
		return nil
	}
	if pf := nm.ProtectFunc(); pf != nil {
		return pf
	}
	return nm.AutoDetectInterfaceFunc()
}

// newFreddieTransport builds a bespoke http.RoundTripper for
// df.iantem.io requests. It bypasses kindling (so smart/fronted/amp's
// race-and-maybe-stdlib-DNS can't time out on the TUN) and binds every
// socket to the physical interface the sing-box NetworkManager has
// detected, using 1.1.1.1 as the DNS resolver. For the test-bench
// scenario where we need freddie reachable from inside the extension
// process even while the TUN is up, this is the most predictable
// path; it would need revisiting before shipping to censored clients
// who need kindling's censorship-bypass transports.
func newFreddieTransport(ctx context.Context) http.RoundTripper {
	bindCtrl := bindEgressToPhysicalInterface(ctx)
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := &net.Dialer{
				Timeout: 5 * time.Second,
				Control: bindCtrl,
			}
			// Ignore the `address` the resolver was pointing at (which
			// on a VPN-up Lantern client is the TUN's fakeip DNS) and
			// force a public recursive resolver reachable over the
			// physical interface.
			return d.DialContext(ctx, "udp", "1.1.1.1:53")
		},
	}
	dialer := &net.Dialer{
		Timeout:  10 * time.Second,
		Resolver: resolver,
		Control:  bindCtrl,
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // long-poll endpoint holds open ~20s
		IdleConnTimeout:       30 * time.Second,
	}
}
