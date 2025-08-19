package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	sbx "github.com/getlantern/sing-box-extensions"
	sbxlog "github.com/getlantern/sing-box-extensions/log"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
	sblog "github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	sbgroup "github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

var (
	tInstance *tunnel
	tAccess   sync.Mutex

	cmdSvr     *libbox.CommandServer
	cmdSvrOnce sync.Once
	cmdSvrErr  error
)

type tunnel struct {
	ctx         context.Context
	lbService   *libbox.BoxService
	cacheFile   adapter.CacheFile
	clashServer *clashapi.Server
	logFactory  sblog.ObservableFactory

	svrFileWatcher *internal.FileWatcher
	reloadAccess   sync.Mutex

	cancel  context.CancelFunc
	closers []io.Closer
}

// establishConnection initializes and starts the VPN tunnel with the provided options and platform interface.
func establishConnection(group, tag string, opts O.Options, dataPath string, platIfce libbox.PlatformInterface) error {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance != nil {
		slog.Warn("Tunnel already opened", "group", group, "tag", tag)
		return errors.New("tunnel already opened")
	}

	slog.Info("Establishing VPN tunnel", "group", group, "tag", tag)

	tInstance = &tunnel{}
	tInstance.ctx, tInstance.cancel = context.WithCancel(sbx.BoxContext())
	if err := tInstance.init(opts, dataPath, platIfce); err != nil {
		slog.Error("Failed to initialize tunnel", "error", err)
		return fmt.Errorf("initializing tunnel: %w", err)
	}

	// we need to set the selected server before starting libbox
	slog.Log(nil, internal.LevelTrace, "Starting cachefile")
	cacheFile := tInstance.cacheFile
	if err := cacheFile.Start(adapter.StartStateInitialize); err != nil {
		slog.Error("Failed to start cache file", "error", err)
		return fmt.Errorf("start cache file: %w", err)
	}
	tInstance.closers = append(tInstance.closers, cacheFile)

	if group == "" { // group is empty, connect to last selected server
		slog.Debug("Connecting to last selected server")
		return tInstance.connect()
	}
	return tInstance.connectTo(group, tag)
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

	if err := startCmdServer(); err != nil {
		return fmt.Errorf("failed to start command server: %w", err)
	}

	t.logFactory = sbxlog.NewFactory(slog.Default().Handler())
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
		if err := t.reloadOptions(svrsPath); err != nil {
			slog.Error("Failed to reload servers", "error", err)
		} else {
			slog.Debug("Servers reloaded successfully")
		}
	})
	t.svrFileWatcher = svrWatcher
	slog.Info("Tunnel initializated")
	return nil
}

func (t *tunnel) connect() (err error) {
	slog.Log(nil, internal.LevelTrace, "Starting libbox service")

	if err = t.lbService.Start(); err != nil {
		t.cacheFile.Close()
		slog.Error("Failed to start libbox service", "error", err)
		return fmt.Errorf("starting libbox service: %w", err)
	}
	// we're using the cmd server to handle libbox.Close, so we don't need to add it to closers
	defer func() {
		if err != nil {
			slog.Warn("Error occurred during connection, closing tunnel", "error", err)
			t.lbService.Close()
			closeTunnel()
		}
	}()
	slog.Debug("Libbox service started")

	t.clashServer = service.FromContext[adapter.ClashServer](t.ctx).(*clashapi.Server)
	cmdSvr.SetService(t.lbService)

	if err = t.svrFileWatcher.Start(); err != nil {
		slog.Error("Failed to start user server file watcher", "error", err)
		return fmt.Errorf("starting user server file watcher: %w", err)
	}
	tInstance.closers = append(tInstance.closers, t.svrFileWatcher)

	slog.Info("Tunnel connection established")
	return nil
}

func (t *tunnel) connectTo(group, tag string) (err error) {
	slog.Log(nil, internal.LevelTrace, "Connecting to server", "group", group, "tag", tag)
	err = t.cacheFile.StoreMode(group)
	if err == nil {
		err = t.cacheFile.StoreSelected(group, tag)
	}
	if err != nil {
		t.cacheFile.Close()
		slog.Error("failed to set selected server", "group", group, "tag", tag, "error", err)
		return fmt.Errorf("set selected server %s.%s: %w", group, tag, err)
	}
	slog.Debug("set selected server", "group", group, "tag", tag)
	return t.connect()
}

func startCmdServer() error {
	cmdSvrOnce.Do(func() {
		slog.Debug("Starting command server")
		cmdSvr = libbox.NewCommandServer(&cmdSvrHandler{}, 64)
		cmdSvrErr = cmdSvr.Start()
	})
	return cmdSvrErr
}

func closeCmdServer() error {
	if err := cmdSvr.Close(); err != nil {
		return fmt.Errorf("closing command server: %w", err)
	}
	return os.Remove(filepath.Join(common.DataPath(), "command.sock"))
}

// closeTunnel stops and cleans up the VPN tunnel instance.
func closeTunnel() error {
	tAccess.Lock()
	if tInstance == nil {
		tAccess.Unlock()
		return nil
	}
	slog.Info("Closing tunnel")
	// copying the mutex is fine here because we're not using it anymore
	//nolint:staticcheck
	t := *tInstance
	t.lbService.Close()
	tInstance = nil
	slog.Log(nil, internal.LevelTrace, "Clearing cmd server tunnel reference")
	cmdSvr.SetService(nil)
	tAccess.Unlock()

	t.cancel()

	var errs []error
	for _, closer := range t.closers {
		slog.Log(nil, internal.LevelTrace, "Closing tunnel resource", "type", fmt.Sprintf("%T", closer))
		errs = append(errs, closer.Close())
	}
	slog.Debug("Tunnel closed")
	return errors.Join(errs...)
}

var errLibboxClosed = errors.New("libbox closed")

func (t *tunnel) reloadOptions(optsPath string) error {
	if t == nil {
		return nil
	}
	t.reloadAccess.Lock()
	defer t.reloadAccess.Unlock()

	select {
	case <-t.ctx.Done():
		return nil
	default:
	}

	content, err := os.ReadFile(optsPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	svrs, err := json.UnmarshalExtendedContext[servers.Servers](sbx.BoxContext(), content)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}
	err = t.updateServers(svrs)
	if t.ctx.Err() != nil {
		return nil // tunnel is closing, ignore the error
	}
	if err != nil && !errors.Is(err, errLibboxClosed) {
		return fmt.Errorf("update server configs: %w", err)
	}
	return nil
}

func (t *tunnel) updateServers(svrs servers.Servers) (err error) {
	ctx := t.ctx
	outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
	endpointMgr := service.FromContext[adapter.EndpointManager](ctx)
	getSelector := func(group string) *sbgroup.Selector {
		out, found := outboundMgr.Outbound(group)
		if !found {
			// Yes, panic. And, yes, it's intentional. The group outbound should always exist if the tunnel
			// is running, so this is a "world no longer makes sense" situation. This should be caught
			// during testing and will not panic in release builds.
			slog.Log(ctx, internal.LevelPanic, "selector group missing", "group", group)
			panic(fmt.Errorf("selector group %q missing", group))
		}
		return out.(*sbgroup.Selector)
	}
	defer func() {
		// the managers will panic with "invalid .* index" if the libbox service is closed when
		// calling Remove or Create. This isn't the best way to determine if the service is closed,
		// but it's the only way to handle this without modifying sing-box.
		if r := recover(); r != nil {
			if v, ok := r.(string); ok && strings.HasPrefix(v, "invalid") && strings.HasSuffix(v, "index") {
				err = errLibboxClosed
			} else {
				panic(r) // re-panic if it's not the expected panic
			}
		}
	}()

	var (
		toRemove   []string
		hasLantern bool
		hasUser    bool
		creator    = &outCreator{
			ctx:        ctx,
			router:     service.FromContext[adapter.Router](ctx),
			logFactory: t.logFactory,
			errs:       make([]error, 0),
		}
	)
	for _, group := range []string{servers.SGLantern, servers.SGUser} {
		sopts := svrs[group]
		newTags := sopts.AllTags()

		switch group {
		case servers.SGLantern:
			hasLantern = len(newTags) > 0
		case servers.SGUser:
			hasUser = len(newTags) > 0
		}

		// determine which tags need to be removed
		groupTags := getSelector(group).All()
		tagsToRemove := make([]string, 0, len(groupTags))
		for _, tag := range groupTags {
			if !slices.Contains(newTags, tag) {
				tagsToRemove = append(tagsToRemove, tag)
			}
		}
		toRemove = slices.Concat(toRemove, tagsToRemove)

		creator.group = group
		creator.succeededTags = make([]string, 0, len(newTags)+1)

		// create/update endpoints and outbounds
		creator.createEndpoints(endpointMgr, sopts.Endpoints)
		creator.createOutbounds(outboundMgr, sopts.Outbounds)

		// create/update urltest and selector for group with the succeeded tags
		autoGroup := groupAutoTag(group)
		opts := urlTestOutbound(autoGroup, creator.succeededTags).Options
		creator.createNew(autoGroup, C.TypeURLTest, opts)

		tags := append(creator.succeededTags, autoGroup)
		opts = selectorOutbound(group, tags).Options
		creator.createNew(group, C.TypeSelector, opts)

	}

	// we have to remove endpoints and outbounds last, otherwise the managers will return an error
	// because the group outbound is dependent on them.
	for _, tag := range toRemove {
		if _, exists := endpointMgr.Get(tag); exists {
			endpointMgr.Remove(tag)
		} else if _, exists := outboundMgr.Outbound(tag); exists {
			outboundMgr.Remove(tag)
		}
	}

	out, found := outboundMgr.Outbound(autoAllTag)
	if !found {
		// see above comment about panic
		slog.Log(nil, internal.LevelPanic, "auto outbound missing", "group", autoAllTag)
		panic(fmt.Errorf("auto outbound %q missing", autoAllTag))
	}
	creator.mgr = outboundMgr
	creator.typ = "outbound"
	updateAuto(creator, out.(adapter.OutboundGroup), hasLantern, hasUser)
	return errors.Join(creator.errs...)
}

func updateAuto(creator *outCreator, auto adapter.OutboundGroup, hasLantern, hasUser bool) {
	slog.Log(nil, internal.LevelTrace, "Updating auto all", "has_lantern", hasLantern, "has_user", hasUser)
	_, isPlaceholder := auto.(*sbgroup.Selector)
	shouldBePlaceholder := !(hasLantern && hasUser)
	if (isPlaceholder && shouldBePlaceholder) || (len(auto.All()) == 2 && !shouldBePlaceholder) {
		return // nothing to do
	}
	if !isPlaceholder && shouldBePlaceholder {
		// create a placeholder
		creator.createNew(autoAllTag, C.TypeSelector, selectorOutbound(autoAllTag, []string{"block"}).Options)
		return
	}

	// otherwise, we need to update the auto outbound with the appropriate tags
	var tags []string
	if hasLantern {
		tags = append(tags, autoLanternTag)
	}
	if hasUser {
		tags = append(tags, autoUserTag)
	}
	creator.createNew(autoAllTag, C.TypeURLTest, urlTestOutbound(autoAllTag, tags).Options)
}

// outCreator is a one-off struct for creating outbounds or endpoints.
type outCreator struct {
	mgr interface {
		Create(context.Context, adapter.Router, sblog.ContextLogger, string, string, any) error
	}
	typ        string
	ctx        context.Context
	router     adapter.Router
	logFactory sblog.ObservableFactory
	group      string

	succeededTags []string
	errs          []error
}

func (o *outCreator) createEndpoints(mgr adapter.EndpointManager, endpoints []O.Endpoint) {
	o.mgr = mgr
	o.typ = "endpoint"
	for _, opts := range endpoints {
		o.createNew(opts.Tag, opts.Type, opts.Options)
	}
}

func (o *outCreator) createOutbounds(mgr adapter.OutboundManager, outbounds []O.Outbound) {
	o.mgr = mgr
	o.typ = "outbound"
	for _, opts := range outbounds {
		o.createNew(opts.Tag, opts.Type, opts.Options)
	}
}

func (o *outCreator) createNew(tag, typ string, opts any) {
	select {
	case <-o.ctx.Done():
		return
	default:
	}

	log := o.logFactory.NewLogger(o.typ + "/" + tag + "[" + typ + "]")
	err := o.mgr.Create(o.ctx, o.router, log, tag, typ, opts)
	if err != nil {
		slog.Error(
			"failed to create "+o.typ,
			slog.String("group", o.group), slog.String("tag", tag),
			slog.String("type", typ), slog.Any("error", err),
		)
		o.errs = append(o.errs, fmt.Errorf("create %s %q: %w", o.typ, tag, err))
	} else {
		slog.Debug("created "+o.typ,
			slog.String("group", o.group), slog.String("tag", tag), slog.String("type", typ),
		)
		o.succeededTags = append(o.succeededTags, tag)
	}
}

type cmdSvrHandler struct {
	libbox.CommandServerHandler
}

func (c *cmdSvrHandler) PostServiceClose() {
	slog.Log(nil, internal.LevelTrace, "Command server received service close request")
	if err := closeTunnel(); err != nil {
		slog.Error("closing tunnel", slog.Any("error", err))
	}
}
