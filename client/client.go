package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/sing-box-extensions/ruleset"

	"github.com/getlantern/radiance/client/boxoptions"
	boxservice "github.com/getlantern/radiance/client/service"
)

var (
	client   *vpnClient
	clientMu sync.Mutex
)

type Options struct {
	DataDir  string
	PlatIfce libbox.PlatformInterface
	// EnableSplitTunneling is the initial state of split tunneling when the service starts
	EnableSplitTunneling bool
}

type VPNClient interface {
	StartVPN() error
	StopVPN() error
	ConnectionStatus() bool
	PauseVPN(dur time.Duration) error
	ResumeVPN()
	SplitTunnelHandler() *SplitTunnel
}

type vpnClient struct {
	boxService         *boxservice.BoxService
	splitTunnelHandler *SplitTunnel
	started            bool
	connected          bool
}

// NewVPNClient creates a new VPNClient instance if one does not already exist, otherwise returns
// the existing instance. logDir is the path where the log file will be written. logDir can be
// set to "stdout" to write logs to stdout. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewVPNClient(opts Options) (VPNClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}

	// TODO: We should be fetching the options from the server.
	logOutput := filepath.Join(opts.DataDir, "logs", "lantern-box.log")
	boxOpts := boxoptions.Options(opts.DataDir, logOutput, SplitTunnelTag, SplitTunnelFormat)
	buf, err := json.Marshal(boxOpts)
	if err != nil {
		return nil, err
	}

	b, err := boxservice.New(string(buf), opts.DataDir, opts.PlatIfce)
	if err != nil {
		return nil, err
	}
	splitTun, err := initSplitTunnel(b.RulesetManager(), opts.DataDir, opts.EnableSplitTunneling)
	if err != nil {
		return nil, fmt.Errorf("split tunnel handler: %w", err)
	}

	client = &vpnClient{
		boxService:         b,
		splitTunnelHandler: splitTun,
	}
	return client, nil
}

// Start starts the VPN client
func (c *vpnClient) StartVPN() error {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c.started {
		return errors.New("VPN client is already running")
	}

	slog.Debug("Starting VPN client")
	if c.boxService == nil {
		return errors.New("box service is not initialized")
	}
	err := c.boxService.Start()
	if err != nil {
		return err
	}

	c.started = true
	return nil
}

// Stop stops the VPN client and closes the TUN device
func (c *vpnClient) StopVPN() error {
	clientMu.Lock()
	defer clientMu.Unlock()
	if !c.started {
		return errors.New("VPN client is not running")
	}

	slog.Debug("Stopping VPN client")
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

// ConnectionStatus returns the connection status of the VPN client
func (c *vpnClient) ConnectionStatus() bool {
	clientMu.Lock()
	defer clientMu.Unlock()
	return c.started && c.connected
}

func (c *vpnClient) setConnectionStatus(connected bool) {
	clientMu.Lock()
	defer clientMu.Unlock()
	c.connected = connected

	// TODO: why is this here?? this is going to create a goroutine every time the connection status
	// changes. Also, the defer gets called immediately after the go routine is created because there
	// are no other statements and you can only recover from panics in the goroutine calling recover.
	// So, this isn't doing anything.

	// // send notifications in a separate goroutine to avoid blocking the Radiance main loop
	// go func() {
	// 	// Recover from panics to avoid crashing the Radiance main loop
	// 	defer func() {
	// 		if r := recover(); r != nil {
	// 			log.Error("Recovered from panic", "error", r)
	// 			reporting.PanicListener(fmt.Sprintf("Recovered from panic: %v", r))
	// 		}
	// 	}()
	// }()
}

func parseConfig(ctx context.Context, configContent string) (option.Options, error) {
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(configContent))
	if err != nil {
		return option.Options{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return options, nil
}

// Pause pauses the VPN client for the specified duration
func (c *vpnClient) PauseVPN(dur time.Duration) error {
	slog.Info("Pausing VPN for", "duration", dur)
	return c.boxService.Pause(dur)
}

// Resume resumes the VPN client
func (c *vpnClient) ResumeVPN() {
	slog.Info("Resuming VPN client")
	c.boxService.Wake()
}

func (c *vpnClient) SplitTunnelHandler() *SplitTunnel {
	return c.splitTunnelHandler
}

const (
	SplitTunnelTag    = "split-tunnel"
	SplitTunnelFormat = constant.RuleSetFormatSource // file will be saved as json
)

type SplitTunnel = ruleset.MutableRuleSet

// initSplitTunnel initializes the split tunnel ruleset handler. It retrieves an existing mutable
// ruleset associated with the SplitTunnelTag or creates a new one if it doesn't exist. dataDir is
// the directory where the ruleset data is stored. The initial state is determined by the enabled
// parameter.
func initSplitTunnel(mgr *ruleset.Manager, dataDir string, enabled bool) (*SplitTunnel, error) {
	rs := mgr.MutableRuleSet(SplitTunnelTag)
	if rs == nil {
		var err error
		rs, err = mgr.NewMutableRuleSet(dataDir, SplitTunnelTag, SplitTunnelFormat, enabled)
		if err != nil {
			return nil, err
		}
	}
	return (*SplitTunnel)(rs), nil
}
