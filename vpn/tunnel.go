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

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"

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

	optsFileWatcher *internal.FileWatcher
	svrFileWatcher  *internal.FileWatcher

	closers []io.Closer
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
	if app.Platform == "android" {
		setupOpts.FixAndroidStack = true
	}
	if err := libbox.Setup(setupOpts); err != nil {
		return fmt.Errorf("setup libbox: %w", err)
	}
	t.logFactory = sbxlog.NewFactory(slog.Default().With("component", "sing-box").Handler())
	service.MustRegister(t.ctx, t.logFactory)
	lb, err := libbox.NewServiceWithContext(t.ctx, string(cfg), platIfce)
	if err != nil {
		return fmt.Errorf("create libbox service: %w", err)
	}
	t.lbService = lb

	if app.Platform == "android" || app.Platform == "ios" {
		libbox.SetMemoryLimit(true)
	}

	// create file watchers for options and server configs
	optsPath := filepath.Join(basePath, app.ConfigFileName)
	optsWatcher := internal.NewFileWatcher(optsPath, func() {
		err := t.reloadOptions(optsPath, unmarshalConfig)
		if err != nil {
			t.log.Error("failed to reload options", slog.Any("error", err))
		}
	})
	t.optsFileWatcher = optsWatcher

	svrsPath := filepath.Join(basePath, app.UserServerFileName)
	svrWatcher := internal.NewFileWatcher(svrsPath, func() {
		err := t.reloadOptions(svrsPath, unmarshal)
		if err != nil {
			t.log.Error("failed to reload user servers", slog.Any("error", err))
		}
	})
	t.svrFileWatcher = svrWatcher
	return nil
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

	tInstance = &tunnel{
		ctx: sbx.BoxContext(),
		log: log,
	}
	if err := tInstance.init(opts, platIfce); err != nil {
		return fmt.Errorf("initialize tunnel: %w", err)
	}
	return tInstance.start()
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

	if err = t.optsFileWatcher.Start(); err != nil {
		return fmt.Errorf("starting config file watcher: %w", err)
	}
	tInstance.closers = append(tInstance.closers, t.optsFileWatcher)

	if err = t.svrFileWatcher.Start(); err != nil {
		return fmt.Errorf("starting user server file watcher: %w", err)
	}
	tInstance.closers = append(tInstance.closers, t.svrFileWatcher)

	return nil
}

func closeTunnel() error {
	tAccess.Lock()
	defer tAccess.Unlock()
	if tInstance == nil {
		return nil
	}
	var t *tunnel
	*t, tInstance = *tInstance, nil

	var errs []error
	for _, closer := range t.closers {
		errs = append(errs, closer.Close())
	}
	return errors.Join(errs...)
}

var errLibboxClosed = errors.New("libbox closed")

func (t *tunnel) reloadOptions(optsPath string, unmarshaler func([]byte) (O.Options, error)) error {
	if t == nil {
		return nil
	}

	t.log.Debug("reloading options")
	content, err := os.ReadFile(optsPath)
	if os.IsNotExist(err) {
		t.log.Debug("config file not found, skipping reload")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	opts, err := unmarshaler(content)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}
	err = t.updateServerConfigs(t.ctx, "", opts.Outbounds, opts.Endpoints)
	if err != nil && !errors.Is(err, errLibboxClosed) {
		return fmt.Errorf("update server configs: %w", err)
	}
	return nil
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

func (t *tunnel) updateServerConfigs(
	ctx context.Context,
	group string,
	outbounds []O.Outbound,
	endpoints []O.Endpoint,
) (err error) {
	defer func() {
		// the managers will panic with "invalid .* index" if the libbox service is closed when
		// calling Remove or Create. This isn't the best way to determine if the service is closed,
		// but it's the only way to handle this without modifying sing-box.
		if r := recover(); r != nil {
			switch r := r.(type) {
			case string:
				if strings.HasPrefix(r, "invalid") && strings.HasSuffix(r, "index") {
					err = errLibboxClosed
				}
			default:
				err = fmt.Errorf("update server configs panic: %v", r)
			}
		}
	}()

	outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
	ob, found := outboundMgr.Outbound(group)
	if !found {
		t.log.Log(ctx, internal.LevelFatal, "outbound group not found", slog.String("group", group))
		return fmt.Errorf("outbound group %q not found", group)
	}
	selector := ob.(*sbgroup.Selector)
	groupTags := selector.All()
	sopts := &serverOptions{outbounds: outbounds, endpoints: endpoints}
	newTags := sopts.tags()

	// determine which tags to need to be removed
	tagsToRemove := make([]string, 0, len(groupTags))
	for _, tag := range groupTags {
		if !slices.Contains(newTags, tag) {
			tagsToRemove = append(tagsToRemove, tag)
		}
	}

	var errs []error
	var succeededTags []string
	router := service.FromContext[adapter.Router](ctx)

	// create/update outbounds
	for _, opts := range sopts.outbounds {
		t.log.Debug("creating outbound", slog.String("tag", opts.Tag), slog.String("type", opts.Type))
		logger := t.logFactory.NewLogger("outbound/" + opts.Tag + "[" + opts.Type + "]")
		err := outboundMgr.Create(ctx, router, logger, opts.Tag, opts.Type, opts.Options)
		if err != nil {
			errs = append(errs, fmt.Errorf("create outbound %q: %w", opts.Tag, err))
		} else {
			succeededTags = append(succeededTags, opts.Tag)
		}
	}
	// create/update endpoints
	endpointMgr := service.FromContext[adapter.EndpointManager](ctx)
	for _, opts := range sopts.endpoints {
		t.log.Debug("creating endpoint", slog.String("tag", opts.Tag), slog.String("type", opts.Type))
		logger := t.logFactory.NewLogger("endpoint/" + opts.Tag + "[" + opts.Type + "]")
		err := endpointMgr.Create(ctx, router, logger, opts.Tag, opts.Type, opts.Options)
		if err != nil {
			errs = append(errs, fmt.Errorf("create endpoint %q: %w", opts.Tag, err))
		} else {
			succeededTags = append(succeededTags, opts.Tag)
		}
	}

	// create a new selector with the succeeded tags
	newSel := newSelector(group, succeededTags)
	logger := t.logFactory.NewLogger("outbound/" + group + "[" + C.TypeSelector + "]")
	err = outboundMgr.Create(ctx, router, logger, group, C.TypeSelector, newSel.Options)
	if err != nil {
		t.log.Error("failed to create selector outbound", slog.String("group", group), slog.String("type", C.TypeSelector), slog.Any("error", err))
		errs = append(errs, fmt.Errorf("create selector outbound %q: %w", group, err))
	}

	// TODO: update urltest group outbounds

	// we have to remove endpoints and outbounds last, otherwise the managers will return an error
	// because the group outbound is dependent on them.
	for _, tag := range tagsToRemove {
		if _, exists := endpointMgr.Get(tag); exists {
			endpointMgr.Remove(tag)
		} else if _, exists := outboundMgr.Outbound(tag); exists {
			outboundMgr.Remove(tag)
		}
	}
	return errors.Join(errs...)
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
