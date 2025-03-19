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
	"reflect"
	"runtime"
	runtimeDebug "runtime/debug"
	"sync"
	"time"
	"unsafe"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/experimental/deprecated"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/experimental/libbox/platform"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"github.com/getlantern/radiance/client/boxoptions"
	"github.com/getlantern/radiance/protocol"
)

// BoxService is a wrapper around libbox.BoxService
type BoxService struct {
	libbox   *libbox.BoxService
	ctx      context.Context
	cancel   context.CancelFunc
	instance *box.Box

	pauseManager pause.Manager
	pauseAccess  sync.Mutex
	pauseTimer   *time.Timer

	defaultOptions option.Options
}

// New creates a new BoxService instance using the provided logOutput and platform interface. The
// platform interface is only used for mobile platforms and should be nil for other platforms.
func New(logOutput string, platIfce libbox.PlatformInterface) (*BoxService, error) {
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	options := boxoptions.Options(logOutput)
	return configureLibboxService(ctx, options, platIfce)
}

func (bs *BoxService) ResetNetwork() {
	bs.instance.Router().ResetNetwork()
}

func (bs *BoxService) NeedWIFIState() bool {
	return bs.instance.Router().NeedWIFIState()
}

func (bs *BoxService) UpdateWIFIState() {
	bs.instance.Network().UpdateWIFIState()
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
	bs.pauseTimer = time.AfterFunc(dur, bs.pauseManager.NetworkWake)
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
	bs.pauseTimer.Stop()
	bs.pauseTimer = nil
}

// ServerConnectConfig represents configuration for connecting to a custom server.
type ServerConnectConfig []byte

// SelectCustomServer replace box service instance by a instance using the
// given config. If the Box service is already running, you'll need to
// stop and start the VPN again so it can use the new instance.
// From the configuration, we're only going to use the Endpoints and Outbounds.
func (bs *BoxService) SelectCustomServer(cfg ServerConnectConfig) error {
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)
	parsedOptions, err := json.UnmarshalExtendedContext[option.Options](ctx, cfg)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	customizedOptions := bs.defaultOptions

	// We will only receive as options Endpoints and Outbounds.
	// We must be able to retrieve the current Endpoints and Outbounds
	// from the current instance, add the received ones. After that
	// we should stop the instance and create a new one. We also
	// need to test this with different OS.
	// There won't be a different customInstance since we can't have multiple ones
	if parsedOptions.Endpoints != nil && len(parsedOptions.Endpoints) > 0 {
		customizedOptions.Endpoints = append(customizedOptions.Endpoints, parsedOptions.Endpoints...)
	}

	if parsedOptions.Outbounds != nil && len(parsedOptions.Outbounds) > 0 {
		customizedOptions.Outbounds = append(customizedOptions.Outbounds, parsedOptions.Outbounds...)
	}

	// adding different routing rules before the latest one
	// and then adding the latest one. Assuming the last rule is responsible for
	// direct traffic
	if bs.defaultOptions.Route != nil && len(bs.defaultOptions.Route.Rules) > 0 {
		customizedOptions.Route.Rules = bs.defaultOptions.Route.Rules[:len(bs.defaultOptions.Route.Rules)-1]
		customizedOptions.Route.Rules = append(customizedOptions.Route.Rules, parsedOptions.Route.Rules...)
		customizedOptions.Route.Rules = append(customizedOptions.Route.Rules, bs.defaultOptions.Route.Rules[len(bs.defaultOptions.Route.Rules)-1])
	}

	if bs.defaultOptions.Route != nil && len(bs.defaultOptions.Route.RuleSet) > 0 {
		customizedOptions.Route.RuleSet = append(customizedOptions.Route.RuleSet, parsedOptions.Route.RuleSet...)
		customizedOptions.Route.RuleSet = append(customizedOptions.Route.RuleSet, bs.defaultOptions.Route.RuleSet...)
	}

	if bs.instance != nil {
		bs.instance.Close()
	}

	ctx, cancel := context.WithCancel(ctx)
	urlTestHistoryStorage := urltest.NewHistoryStorage()
	ctx = service.ContextWithPtr(ctx, urlTestHistoryStorage)
	instance, err := box.New(box.Options{
		Options: customizedOptions,
		Context: ctx,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("create service: %w", err)
	}

	bs.instance = instance
	bs.cancel = cancel

	return nil
}

// DeselectCustomServer stops the current instance and replace it by
// the default instance.
func (bs *BoxService) DeselectCustomServer() error {
	if bs.instance != nil {
		bs.instance.Close()
	}

	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	ctx, cancel := context.WithCancel(ctx)
	urlTestHistoryStorage := urltest.NewHistoryStorage()
	ctx = service.ContextWithPtr(ctx, urlTestHistoryStorage)

	instance, err := box.New(box.Options{
		Options: bs.defaultOptions,
		Context: ctx,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("create service: %w", err)
	}
	bs.instance = instance
	bs.cancel = cancel
	return nil
}

// configureLibboxService creates a new libbox.BoxService instance using the provided context, options, and
// platformInterface. platIfce is only used for mobile platforms and will be ignored on other platforms
// in which case it should be nil.
func configureLibboxService(ctx context.Context, options option.Options, platIfce libbox.PlatformInterface) (*BoxService, error) {
	// This should only be used until sing-box is updated to allow wrapping a box.Box instance
	// (if that ever happens).

	////////////////////////////////////////////////////////////////////////
	// Do not modify the following code unless you know what you're doing //
	////////////////////////////////////////////////////////////////////////

	lbService, err := newLibbox(platIfce)
	if err != nil {
		return nil, err
	}

	lbctxptr := getFieldPtr[context.Context](lbService, "ctx")

	var logWriter log.PlatformWriter
	if runtime.GOOS == "ios" || runtime.GOOS == "android" {
		lbCtx := *lbctxptr

		deprecatedManager := service.FromContext[deprecated.Manager](lbCtx)
		service.MustRegister[deprecated.Manager](ctx, deprecatedManager)

		pIface := service.FromContext[platform.Interface](lbCtx)
		service.MustRegister[platform.Interface](ctx, pIface)
		logWriter = pIface.(log.PlatformWriter)
	}

	// TODO: Do we want to use the filemanager service?
	//
	//	ctx = filemanager.WithDefault(ctx, sWorkingPath, sTempPath, sUserID, sGroupID)
	// ctx = service.ContextWith(ctx, filemgr)

	ctx, cancel := context.WithCancel(ctx)
	urlTestHistoryStorage := urltest.NewHistoryStorage()
	ctx = service.ContextWithPtr(ctx, urlTestHistoryStorage)

	instance, err := box.New(box.Options{
		Options:           options,
		Context:           ctx,
		PlatformLogWriter: logWriter,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create service: %w", err)
	}

	lbcancelptr := getFieldPtr[context.CancelFunc](lbService, "cancel")
	lbboxptr := getFieldPtr[*box.Box](lbService, "instance")
	lburlthsptr := getFieldPtr[*urltest.HistoryStorage](lbService, "urlTestHistoryStorage")
	lbpauseptr := getFieldPtr[pause.Manager](lbService, "pauseManager")
	lbclashptr := getFieldPtr[adapter.ClashServer](lbService, "clashServer")

	*lbcancelptr = cancel
	*lbctxptr = ctx
	*lbboxptr = instance

	*lburlthsptr = urlTestHistoryStorage
	*lbpauseptr = service.FromContext[pause.Manager](ctx)
	*lbclashptr = service.FromContext[adapter.ClashServer](ctx)

	runtimeDebug.FreeOSMemory()
	return &BoxService{
		libbox:         lbService,
		ctx:            ctx,
		cancel:         cancel,
		instance:       instance,
		defaultOptions: options,
		pauseManager:   service.FromContext[pause.Manager](ctx),
	}, nil
}

// DO NOT USE THIS FUNCTION. It's to be used exclusively by newlibbox
//
// This is a hack to access fields we need to modify in the libbox.BoxService struct. The only
// reason we do this is because libbox does not provide a way to wrap an existing box.Box instance.
// Once/if libbox is updated to allow it, this function should be removed.
func getFieldPtr[T any](box any, fieldName string) *T {
	val := reflect.ValueOf(box).Elem()
	field := val.FieldByName(fieldName)
	return (*T)(unsafe.Pointer(field.UnsafeAddr()))
}
