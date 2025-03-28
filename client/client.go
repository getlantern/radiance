package client

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/client/boxoptions"
	boxservice "github.com/getlantern/radiance/client/service"
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
	boxService *boxservice.BoxService
}

// NewVPNClient creates a new VPNClient instance if one does not already exist, otherwise returns
// the existing instance. logDir is the path where the log file will be written. logDir can be
// set to "stdout" to write logs to stdout. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewVPNClient(logDir string, platIfce libbox.PlatformInterface) (VPNClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}

	// TODO: We should be fetching the options from the server.
	logOutput := filepath.Join(logDir, "lantern-box.log")
	opts := boxoptions.Options(logOutput)
	buf, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}

	b, err := boxservice.New(string(buf), logDir, platIfce)
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
	err := c.boxService.Start()
	if err != nil {
		return err
	}
	return nil
}

// Stop stops the VPN client and closes the TUN device
func (c *vpnClient) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	var err error
	go func() {
		err = c.boxService.Close()
		cancel()
	}()
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("box did not stop in time")
	}
	return err
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
	return c.boxService.Pause(dur)
}

// Resume resumes the VPN client
func (c *vpnClient) Resume() {
	c.boxService.Wake()
}
