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
	err := c.boxService.defaultInstance.Start()
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
		err = c.boxService.defaultInstance.Close()
		cancel()
	}()
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("box did not stop in time")
	}
	return err
}

type boxService struct {
	ctx             context.Context
	cancel          context.CancelFunc
	currentInstance *box.Box
	defaultInstance *box.Box

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

	ctx, cancel := context.WithCancel(ctx)
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: boxoptions.Options(logOutput),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create service: %w", err)
	}
	return &boxService{
		ctx:             ctx,
		cancel:          cancel,
		defaultInstance: instance,
		currentInstance: instance,
		pauseManager:    service.FromContext[pause.Manager](ctx),
	}, nil
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
func (c *vpnClient) SelectCustomServer(cfg ServerConnectConfig) error {
	options, err := json.UnmarshalExtended[option.Options]([]byte(cfg))
	if err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	customInstance, err := box.New(box.Options{
		Options: options,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	if c.boxService.currentInstance != nil {
		c.boxService.currentInstance.Close()
	}

	c.boxService.currentInstance = customInstance
	return nil
}

func (c *vpnClient) DeselectCustomServer() error {
	if c.boxService.currentInstance != nil {
		c.boxService.currentInstance.Close()
	}

	c.boxService.currentInstance = c.boxService.defaultInstance
	return nil
}
