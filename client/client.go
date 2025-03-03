package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/getlantern/radiance/client/boxoptions"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
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
}

type vpnClient struct {
	boxService *boxService
}

// NewVPNClient creates a new VPNClient instance if one does not already exist, otherwise returns
// the existing instance.
func NewVPNClient() (VPNClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}
	b, err := newBoxService()
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
	ctx          context.Context
	cancel       context.CancelFunc
	instance     *box.Box
	pauseManager pause.Manager
}

func newBoxService() (*boxService, error) {
	// ***** REGISTER NEW PROTOCOL HERE *****
	ctx := box.Context(
		context.Background(),
		include.InboundRegistry(),
		include.OutboundRegistry(),
		include.EndpointRegistry(),
	)
	ctx, cancel := context.WithCancel(ctx)
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: boxoptions.Options(),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create service: %w", err)
	}
	return &boxService{
		ctx:          ctx,
		cancel:       cancel,
		instance:     instance,
		pauseManager: service.FromContext[pause.Manager](ctx),
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
