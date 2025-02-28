package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/getlantern/golog"

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

	glog = golog.LoggerFor("box")
)

type ClientOptions = option.Options

type VPNClient interface {
	Start() error
	Stop() error
	Pause(dur time.Duration) error
	Resume()
}

type vpnClient struct {
	boxService *boxService
}

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

func (c *vpnClient) Pause(dur time.Duration) error {
	if c.boxService.pauseManager.IsNetworkPaused() {
		return errors.New("network is already paused")
	}
	c.boxService.pauseManager.NetworkPause()
	time.AfterFunc(dur, c.boxService.pauseManager.NetworkWake)
	return nil
}

func (c *vpnClient) Resume() {
	if c.boxService.pauseManager.IsNetworkPaused() {
		c.boxService.pauseManager.NetworkWake()
	}
}

func readConfigFile(path string) (string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading config file: %v", err)
	}
	return string(buf), nil
}

func parseConfig(ctx context.Context, configContent string) (option.Options, error) {
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(configContent))
	if err != nil {
		return option.Options{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return options, nil
}

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

// old code
// func newBox() (*box.Box, error) {
// 	glog.Debug("Creating box")
//
// 	// ***** REGISTER NEW PROTOCOL HERE *****
// 	ctx := box.Context(
// 		context.Background(),
// 		include.InboundRegistry(),
// 		include.OutboundRegistry(),
// 		include.EndpointRegistry(),
// 	)
// 	glog.Debug("registering algeneva protocol")
// 	outboundRegistry := service.FromContext[adapter.OutboundRegistry](ctx)
// 	algeneva.RegisterOutbound(outboundRegistry.(*outbound.Registry))
// 	// see https://github.com/SagerNet/sing-box/blob/v1.11.3/protocol/http/outbound.go#L22
//
// 	boxOpts := box.Options{
// 		Options: boxOptions,
// 		Context: ctx,
// 	}
// 	return box.New(boxOpts)
// }
