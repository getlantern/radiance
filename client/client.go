package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/getlantern/radiance/client/boxoptions"
	"github.com/getlantern/radiance/protocol"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
)

var (
	client   *vpnClient
	clientMu sync.Mutex
)

type VPNClient interface {
	Start() error
	Stop() error
	Pause(dur time.Duration) error
	Resume()
	SelectCustomServer(cfg ServerConnectConfig) error
	DeselectCustomServer() error
}

type vpnClient struct {
	boxService *boxService
}

// NewVPNClient creates a new VPNClient instance if one does not already exist, otherwise returns
// the existing instance. logOutput is the path where the log file will be written. logOutput can be
// set to "stdout" to write logs to stdout.
func NewVPNClient(logOutput string) (VPNClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}
	b, err := newBoxService(logOutput)
	if err != nil {
		return nil, err
	}
	client = &vpnClient{
		boxService: b,
	}
	return client, nil
}

// Start starts the VPN client
func (c *vpnClient) Start() error {
	if c.boxService == nil {
		return errors.New("box service is not initialized")
	}
	err := c.boxService.instance.Start()
	if err != nil {
		return err
	}
	return nil
}

// Stop stops the VPN client and closes the TUN device
func (c *vpnClient) Stop() error {
	ctx, cancel := context.WithTimeout(c.boxService.ctx, time.Second*30)
	var err error
	go func() {
		err = c.boxService.instance.Close()
		cancel()
	}()
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("box did not stop in time")
	}
	return err
}

type boxService struct {
	ctx            context.Context
	cancel         context.CancelFunc
	instance       *box.Box
	defaultOptions option.Options

	pauseManager pause.Manager
}

func newBoxService(logOutput string) (*boxService, error) {
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	options := boxoptions.Options(logOutput)
	instance, cancel, err := newInstanceWithOptions(ctx, options)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create service: %w", err)
	}
	return &boxService{
		ctx:            ctx,
		cancel:         cancel,
		instance:       instance,
		defaultOptions: options,
		pauseManager:   service.FromContext[pause.Manager](ctx),
	}, nil
}

func newInstanceWithOptions(ctx context.Context, options option.Options) (*box.Box, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(ctx)
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: options,
	})
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("create service: %w", err)
	}
	return instance, cancel, nil
}

func parseConfig(ctx context.Context, configContent string) (option.Options, error) {
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(configContent))
	if err != nil {
		return option.Options{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return options, nil
}

// Pause pauses the VPN client for the specified duration
func (c *vpnClient) Pause(dur time.Duration) error {
	if c.boxService.pauseManager.IsNetworkPaused() {
		return errors.New("network is already paused")
	}
	c.boxService.pauseManager.NetworkPause()
	time.AfterFunc(dur, c.boxService.pauseManager.NetworkWake)
	return nil
}

// Resume resumes the VPN client
func (c *vpnClient) Resume() {
	if c.boxService.pauseManager.IsNetworkPaused() {
		c.boxService.pauseManager.NetworkWake()
	}
}

// ServerConnectConfig represents configuration for connecting to a custom server.
type ServerConnectConfig []byte

// SelectCustomServer replace box service instance by a instance using the
// given config. If the Box service is already running, you'll need to
// stop and start the VPN again so it can use the new instance.
// From the configuration, we're only going to use the Endpoints and Outbounds.
func (c *vpnClient) SelectCustomServer(cfg ServerConnectConfig) error {
	parsedOptions, err := json.UnmarshalExtended[option.Options](cfg)
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	customizedOptions := c.boxService.defaultOptions

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
	if c.boxService.defaultOptions.Route != nil && len(c.boxService.defaultOptions.Route.Rules) > 0 {
		customizedOptions.Route.Rules = c.boxService.defaultOptions.Route.Rules[:len(c.boxService.defaultOptions.Route.Rules)-1]
		customizedOptions.Route.Rules = append(customizedOptions.Route.Rules, parsedOptions.Route.Rules...)
		customizedOptions.Route.Rules = append(customizedOptions.Route.Rules, c.boxService.defaultOptions.Route.Rules[len(c.boxService.defaultOptions.Route.Rules)-1])
	}

	if c.boxService.defaultOptions.Route != nil && len(c.boxService.defaultOptions.Route.RuleSet) > 0 {
		customizedOptions.Route.RuleSet = append(customizedOptions.Route.RuleSet, parsedOptions.Route.RuleSet...)
		customizedOptions.Route.RuleSet = append(customizedOptions.Route.RuleSet, c.boxService.defaultOptions.Route.RuleSet...)
	}

	if c.boxService.instance != nil {
		c.boxService.instance.Close()
	}

	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)
	instance, cancel, err := newInstanceWithOptions(ctx, customizedOptions)
	if err != nil {
		return fmt.Errorf("failed to create box service: %w", err)
	}

	c.boxService.instance = instance
	c.boxService.cancel = cancel

	return nil
}

// DeselectCustomServer stops the current instance and replace it by
// the default instance.
func (c *vpnClient) DeselectCustomServer() error {
	if c.boxService.instance != nil {
		c.boxService.instance.Close()
	}

	instance, err := newBoxService(c.boxService.defaultOptions.Log.Output)
	if err != nil {
		return fmt.Errorf("failed to create box service: %w", err)
	}
	c.boxService = instance
	return nil
}
