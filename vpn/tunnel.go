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

	lcommon "github.com/getlantern/common"
	sbx "github.com/getlantern/sing-box-extensions"
	sbxlog "github.com/getlantern/sing-box-extensions/log"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/servers"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
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
	clashServer *clashapi.Server
	log         *slog.Logger
	logFactory  sblog.ObservableFactory

	svrFileWatcher *internal.FileWatcher
	reloadAccess   sync.Mutex

	cancel  context.CancelFunc
	closers []io.Closer
}

func openTunnel(opts O.Options, platIfce libbox.PlatformInterface) error {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance != nil {
		return errors.New("tunnel already opened")
	}

	log := slog.Default().With("component", "tunnel")

	cmdSvrOnce.Do(func() {
		cmdSvr = libbox.NewCommandServer(&cmdSvrHandler{log: log}, 64)
		cmdSvrErr = cmdSvr.Start()
	})
	if cmdSvrErr != nil {
		log.Error("failed to start command server", slog.Any("error", cmdSvrErr))
		return fmt.Errorf("failed to start command server: %w", cmdSvrErr)
	}

	tInstance = &tunnel{log: log}
	tInstance.ctx, tInstance.cancel = context.WithCancel(sbx.BoxContext())
	if err := tInstance.init(opts, platIfce); err != nil {
		return fmt.Errorf("initializing tunnel: %w", err)
	}
	return tInstance.start()
}

func (t *tunnel) init(opts O.Options, platIfce libbox.PlatformInterface) error {
	cfg, err := json.MarshalContext(t.ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to marshal options: %w", err)
	}

	// setup libbox service
	basePath := common.DataPath()
	setupOpts := &libbox.SetupOptions{
		BasePath:    basePath,
		WorkingPath: basePath,
		TempPath:    filepath.Join(basePath, "temp"),
	}
	if common.Platform == "android" {
		setupOpts.FixAndroidStack = true
	}
	if err := libbox.Setup(setupOpts); err != nil {
		return fmt.Errorf("setup libbox: %w", err)
	}
	t.logFactory = sbxlog.NewFactory(slog.Default().With("service", "sing-box").Handler())
	service.MustRegister(t.ctx, t.logFactory)
	lb, err := libbox.NewServiceWithContext(t.ctx, string(cfg), platIfce)
	if err != nil {
		return fmt.Errorf("create libbox service: %w", err)
	}
	t.lbService = lb

	if common.Platform == "android" || common.Platform == "ios" {
		libbox.SetMemoryLimit(true)
	}

	// create file watcher for server changes
	svrsPath := filepath.Join(basePath, common.ServersFileName)
	svrWatcher := internal.NewFileWatcher(svrsPath, func() {
		if err := t.reloadOptions(svrsPath); err != nil {
			t.log.Error("failed to reload user servers", slog.Any("error", err))
		}
	})
	t.svrFileWatcher = svrWatcher
	return nil
}

func (t *tunnel) start() (err error) {
	if err = t.lbService.Start(); err != nil {
		return fmt.Errorf("starting libbox service: %w", err)
	}
	// we're using the cmd server to handle libbox.Close, so we don't need to add it to closers
	defer func() {
		if err != nil {
			t.lbService.Close()
			closeTunnel()
		}
	}()
	t.clashServer = service.FromContext[adapter.ClashServer](t.ctx).(*clashapi.Server)
	cmdSvr.SetService(t.lbService)

	if err = t.svrFileWatcher.Start(); err != nil {
		return fmt.Errorf("starting user server file watcher: %w", err)
	}
	tInstance.closers = append(tInstance.closers, t.svrFileWatcher)

	return nil
}

func closeTunnel() error {
	tAccess.Lock()
	if tInstance == nil {
		tAccess.Unlock()
		return nil
	}
	var t *tunnel
	// copying the mutex is fine here because we're not using it anymore
	//nolint:staticcheck
	*t, tInstance = *tInstance, nil
	tAccess.Unlock()

	t.cancel()

	var errs []error
	for _, closer := range t.closers {
		errs = append(errs, closer.Close())
	}
	return errors.Join(errs...)
}

func unmarshal(buf []byte) (O.Options, error) {
	return json.UnmarshalExtendedContext[O.Options](sbx.BoxContext(), buf)
}

func unmarshalConfig(buf []byte) (O.Options, error) {
	config, err := json.UnmarshalExtendedContext[lcommon.ConfigResponse](sbx.BoxContext(), buf)
	if err != nil {
		return O.Options{}, err
	}
	return config.Options, nil
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

	t.log.Debug("reloading options")
	content, err := os.ReadFile(optsPath)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
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
			t.log.Log(ctx, internal.LevelPanic, "selector group missing", slog.String("group", group))
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
		allTags  []string
		toRemove []string
		creater  = &outCreater{
			log:        t.log,
			ctx:        ctx,
			router:     service.FromContext[adapter.Router](ctx),
			logFactory: t.logFactory,
			errs:       make([]error, 0),
		}
	)
	for _, group := range []string{servers.SGLantern, servers.SGUser} {
		sopts := svrs[group]
		newTags := sopts.AllTags()

		// determine which tags need to be removed
		groupTags := getSelector(group).All()
		tagsToRemove := make([]string, 0, len(groupTags))
		for _, tag := range groupTags {
			if !slices.Contains(newTags, tag) {
				tagsToRemove = append(tagsToRemove, tag)
			}
		}
		toRemove = slices.Concat(toRemove, tagsToRemove)

		creater.mgr = endpointMgr
		creater.typ = "endpoint"
		creater.group = group
		creater.succeededTags = make([]string, 0, len(newTags))
		// create/update endpoints
		for _, opts := range sopts.Endpoints {
			creater.createNew(opts.Tag, opts.Type, opts.Options)
		}

		// create/update outbounds
		creater.mgr = outboundMgr
		creater.typ = "outbound"
		for _, opts := range sopts.Outbounds {
			creater.createNew(opts.Tag, opts.Type, opts.Options)
		}
		allTags = slices.Concat(allTags, newTags)

		// create url test outbounds for group with the succeeded tags
		autoGroup := groupAutoTag(group)
		opts := urlTestOutbound(autoGroup, creater.succeededTags).Options
		creater.createNew(autoGroup, C.TypeURLTest, opts)

		// create a new selector with the succeeded tags
		tags := append(creater.succeededTags, autoGroup)
		opts = selectorOutbound(group, tags).Options
		creater.createNew(group, C.TypeSelector, opts)

	}
	// create new auto-all outbound
	opts := urlTestOutbound(modeAutoAll, allTags).Options
	creater.createNew(modeAutoAll, C.TypeURLTest, opts)

	// we have to remove endpoints and outbounds last, otherwise the managers will return an error
	// because the group outbound is dependent on them.
	for _, tag := range toRemove {
		if _, exists := endpointMgr.Get(tag); exists {
			endpointMgr.Remove(tag)
		} else if _, exists := outboundMgr.Outbound(tag); exists {
			outboundMgr.Remove(tag)
		}
	}
	return errors.Join(creater.errs...)
}

// outCreater is a one-off struct for creating outbounds or endpoints.
type outCreater struct {
	mgr interface {
		Create(context.Context, adapter.Router, sblog.ContextLogger, string, string, any) error
	}
	typ string
	log *slog.Logger

	ctx        context.Context
	router     adapter.Router
	logFactory sblog.ObservableFactory
	group      string

	succeededTags []string
	errs          []error
}

func (o *outCreater) createNew(tag, typ string, opts any) {
	select {
	case <-o.ctx.Done():
		return
	default:
	}

	log := o.logFactory.NewLogger(o.typ + "/" + tag + "[" + typ + "]")
	err := o.mgr.Create(o.ctx, o.router, log, tag, typ, opts)
	if err != nil {
		o.log.Error(
			"failed to create "+o.typ,
			slog.String("group", o.group), slog.String("tag", tag),
			slog.String("type", typ), slog.Any("error", err),
		)
		o.errs = append(o.errs, fmt.Errorf("create %s %q: %w", o.typ, tag, err))
	} else {
		o.log.Debug("created "+o.typ,
			slog.String("group", o.group), slog.String("tag", tag), slog.String("type", typ),
		)
		o.succeededTags = append(o.succeededTags, tag)
	}
}

type cmdSvrHandler struct {
	libbox.CommandServerHandler
	log *slog.Logger
}

func (c *cmdSvrHandler) PostServiceClose() {
	if err := closeTunnel(); err != nil {
		c.log.Error("closing tunnel", slog.Any("error", err))
	}
}
